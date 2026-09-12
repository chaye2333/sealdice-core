package dice

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"sealdice-core/dice/service"
)

// 本文件实现「身份与日志绑定」，用于把 QQ 官方机器人得到的新身份
// （OpenQQ:<UIN>-<MemberOpenID>、OpenQQ-Group:<UIN>-<GroupOpenID>）
// 指回迁移之前的旧身份（NapCat 等 OneBot 实现下的 QQ:12345、QQ-Group:789）。
//
// 设计要点：
//  1. 绑定只做「别名回退读取」，不搬运、不修改任何历史数据，.unbind 之后立刻恢复原状。
//  2. 只有 QQ 官方机器人端点会使用绑定结果，OneBot 等平台的行为完全不变。
//  3. 权限判定始终使用真实的新身份，绝不使用绑定到的旧身份，避免越权。
//  4. 归属靠**验证码**证明：验证码发给被声明的旧账号（民间 bot 私聊，或寄 QQ 邮箱），
//     只有真正持有它的人才能确认，因此无法抢绑。答题那种"猜卡名"的方式已被移除。

const (
	// identityBindStoreFilename 绑定关系落盘文件名，位于 data/<骰子名>/ 下。
	identityBindStoreFilename = "identity-bindings.json"

	// identityBindStoreVersion 落盘格式版本。
	// v2 起索引改为「新旧 ID 双向」，旧文件仍然能读（Key 字段被忽略后重建）。
	identityBindStoreVersion = 2

	// identityBindMinCooldownSec / identityBindMaxCooldownSec 冷却时间取值范围（秒）。
	identityBindMinCooldownSec = 1
	identityBindMaxCooldownSec = 86400

	// identityBindGroupPrefix / identityBindUserPrefix 旧 OneBot 身份前缀。
	identityBindGroupPrefix = "QQ-Group:"
	identityBindUserPrefix  = "QQ:"
)

// identityBindAction 绑定类型。
type identityBindAction string

const (
	identityBindActionUser  identityBindAction = "user"
	identityBindActionGroup identityBindAction = "group"
)

// identityBindEndpoint 描述一个身份（旧 QQ 号或新 OpenID）。
type identityBindEndpoint struct {
	Platform  string `json:"platform"`
	Protocol  string `json:"protocol"`
	GroupID   string `json:"groupId"`
	UserID    string `json:"userId"`
	GroupName string `json:"groupName,omitempty"`
	UserName  string `json:"userName,omitempty"`
}

// identityBindRecord 一条绑定关系。
type identityBindRecord struct {
	Action  identityBindAction   `json:"action"`
	Key     string               `json:"key"`
	New     identityBindEndpoint `json:"new"`
	Old     identityBindEndpoint `json:"old"`
	Created int64                `json:"createdAt"`
	Creator string               `json:"createdBy"`
}

// identityBindStoreData 落盘结构。
type identityBindStoreData struct {
	Version int                   `json:"version"`
	SavedAt int64                 `json:"savedAt"`
	Records []*identityBindRecord `json:"records"`
}

// identityBindStore 绑定关系的内存索引 + 落盘。
type identityBindStore struct {
	mu      sync.RWMutex
	byKey   map[string]*identityBindRecord
	loaded  bool
	lastErr error
}

// identityBindSession 一次等待确认的绑定会话。
//
// 保留它只是为了 .bind cancel 有对象可清；验证码挑战本身在 globalIdentityBindCodes 里。
type identityBindSession struct {
	Action  identityBindAction
	New     identityBindEndpoint
	Old     identityBindEndpoint
	Created int64
}

// identityBindLastAttempt 记录上一次发起绑定的时间，用于冷却。
type identityBindLastAttempt struct {
	At int64
}

var (
	globalIdentityBindSessions    SyncMap[string, *identityBindSession]
	globalIdentityBindLastAttempt SyncMap[string, *identityBindLastAttempt]
)

// identityBindStoreOf 取当前骰子的绑定存储。
// 正常情况下由 Dice.Init 初始化；这里额外兜底，避免未初始化的实例直接 panic。
func identityBindStoreOf(d *Dice) *identityBindStore {
	if d == nil {
		return &identityBindStore{}
	}
	if d.IdentityBindStore == nil {
		d.IdentityBindStore = &identityBindStore{}
	}
	return d.IdentityBindStore
}

// ---------- 配置读取 ----------

// identityBindSupported 判断该端点是否支持身份绑定。
// 只有 QQ 官方机器人需要这个功能，其它平台（含 OneBot）保持原样。
func identityBindSupported(ep *EndPointInfo) bool {
	if ep == nil {
		return false
	}
	return ep.Platform == "QQ" && ep.ProtocolType == "official"
}

// identityBindEnabled 是否开启了身份绑定功能。
func identityBindEnabled(d *Dice) bool {
	return d != nil && d.Config.IdentityBindEnable
}

// identityBindCooldown 取配置的冷却秒数并收敛到合法范围。
func identityBindCooldown(d *Dice) time.Duration {
	if d == nil {
		return 0
	}
	sec := d.Config.IdentityBindCooldownSec
	if sec <= 0 {
		return 0
	}
	if sec < identityBindMinCooldownSec {
		sec = identityBindMinCooldownSec
	}
	if sec > identityBindMaxCooldownSec {
		sec = identityBindMaxCooldownSec
	}
	return time.Duration(sec) * time.Second
}

// identityBindFormatDuration 把剩余时长格式化成「12 小时 3 分」这样的可读文本。
func identityBindFormatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Seconds())
	hours := total / 3600
	minutes := (total % 3600) / 60
	switch {
	case hours > 0 && minutes > 0:
		return fmt.Sprintf("%d 小时 %d 分", hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%d 小时", hours)
	case minutes > 0:
		return fmt.Sprintf("%d 分", minutes)
	default:
		return fmt.Sprintf("%d 秒", total)
	}
}

// ---------- 身份字符串处理 ----------

// normalizeIdentityBindUser 把用户输入的旧 QQ 号规范成 QQ:12345。
func normalizeIdentityBindUser(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, identityBindUserPrefix)
	if value == "" {
		return "", errors.New("没有填写旧 QQ 号")
	}
	if strings.HasPrefix(value, "-Group:") {
		return "", fmt.Errorf("%q 看起来是群号，请填写个人 QQ 号", raw)
	}
	if !isAllDigits(value) {
		return "", fmt.Errorf("旧 QQ 号 %q 不是纯数字", raw)
	}
	return identityBindUserPrefix + value, nil
}

// normalizeIdentityBindGroup 把用户输入的旧群号规范成 QQ-Group:789。
func normalizeIdentityBindGroup(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, identityBindGroupPrefix)
	if value == "" {
		return "", errors.New("没有填写旧群号")
	}
	if !isAllDigits(value) {
		return "", fmt.Errorf("旧群号 %q 不是纯数字", raw)
	}
	return identityBindGroupPrefix + value, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---------- 存储 ----------

func (s *identityBindStore) storePath(d *Dice) string {
	dir := ""
	if d != nil {
		dir = d.BaseConfig.DataDir
	}
	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join("data", "default")
	}
	return filepath.Join(dir, identityBindStoreFilename)
}

// ensureLoaded 惰性加载落盘数据。读取失败时记录错误并继续使用内存数据，
// 避免因为一个坏文件让骰子无法启动。
func (s *identityBindStore) ensureLoaded(d *Dice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return
	}
	s.loaded = true
	if s.byKey == nil {
		s.byKey = map[string]*identityBindRecord{}
	}
	data, err := os.ReadFile(s.storePath(d))
	if err != nil {
		if !os.IsNotExist(err) {
			s.lastErr = err
		}
		return
	}
	if len(data) == 0 {
		return
	}
	var payload identityBindStoreData
	if err := json.Unmarshal(data, &payload); err != nil {
		s.lastErr = err
		return
	}
	for _, record := range payload.Records {
		keys := identityBindRecordKeys(record)
		if len(keys) == 0 {
			continue
		}
		// 老版本文件里 Key 形如 "user|QQ:123"，这里忽略它，一律按新旧 ID 重建索引。
		record.Key = identityBindRecordKey(record)
		for _, key := range keys {
			s.byKey[key] = record
		}
	}
}

// saveLocked 落盘，调用方必须已持有写锁。
func (s *identityBindStore) saveLocked(d *Dice) error {
	// 一条绑定在索引里有两份，落盘时要按记录去重。
	records := make([]*identityBindRecord, 0, len(s.byKey))
	seen := map[*identityBindRecord]bool{}
	for _, record := range s.byKey {
		if record == nil || seen[record] {
			continue
		}
		seen[record] = true
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return identityBindRecordKey(records[i]) < identityBindRecordKey(records[j])
	})
	payload := identityBindStoreData{
		Version: identityBindStoreVersion,
		SavedAt: time.Now().Unix(),
		Records: records,
	}
	data, err := json.MarshalIndent(&payload, "", "  ")
	if err != nil {
		s.lastErr = err
		return err
	}
	path := s.storePath(d)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.lastErr = err
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		s.lastErr = err
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		s.lastErr = err
		return err
	}
	s.lastErr = nil
	return nil
}

// isOfficialQQID 判断一个 ID 是否来自 QQ 官方机器人。
//
// 这里刻意**不看 EndPoint**，只看 ID 前缀：绑定查询必须能从两侧都能命中，
// 如果依赖「当前端点是不是官方」，旧号（民间 bot）那一侧就永远查不到记录，
// 双向共享也就无从谈起。
func isOfficialQQID(id string) bool {
	return strings.HasPrefix(id, officialQQUserIDPrefix) || strings.HasPrefix(id, officialQQGroupIDPrefix)
}

// identityBindRecordEndpointID 取一条记录在指定类型下的「新身份」ID。
func identityBindRecordEndpointID(record *identityBindRecord) string {
	if record == nil {
		return ""
	}
	if record.Action == identityBindActionGroup {
		return record.New.GroupID
	}
	return record.New.UserID
}

// identityBindRecordOldID 取一条记录在指定类型下的「旧身份」ID。
func identityBindRecordOldID(record *identityBindRecord) string {
	if record == nil {
		return ""
	}
	if record.Action == identityBindActionGroup {
		return record.Old.GroupID
	}
	return record.Old.UserID
}

// identityBindRecordKey 一条记录的主键，固定用「新身份」。
// 老版本按 "user|QQ:123" 存过 Key，这里一律重新推导，不做兼容读取。
func identityBindRecordKey(record *identityBindRecord) string {
	return identityBindRecordEndpointID(record)
}

// identityBindRecordKeys 一条记录要建立的全部索引键：新旧两侧都要能查到。
func identityBindRecordKeys(record *identityBindRecord) []string {
	ids := []string{identityBindRecordEndpointID(record), identityBindRecordOldID(record)}
	keys := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		keys = append(keys, id)
	}
	return keys
}

// put 写入/覆盖一条绑定（新旧两个 ID 都建索引）并落盘。
func (s *identityBindStore) put(d *Dice, record *identityBindRecord) error {
	if record == nil {
		return errors.New("绑定内容为空")
	}
	keys := identityBindRecordKeys(record)
	if len(keys) == 0 {
		return errors.New("绑定内容缺失关键 ID")
	}
	record.Key = identityBindRecordKey(record)
	s.ensureLoaded(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey == nil {
		s.byKey = map[string]*identityBindRecord{}
	}
	for _, key := range keys {
		s.byKey[key] = record
	}
	return s.saveLocked(d)
}

// delete 按记录删除（新旧两个索引一起清掉）并落盘，返回是否真的删掉了。
func (s *identityBindStore) delete(d *Dice, record *identityBindRecord) (bool, error) {
	if record == nil {
		return false, nil
	}
	keys := identityBindRecordKeys(record)
	if len(keys) == 0 {
		return false, nil
	}
	s.ensureLoaded(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	for _, key := range keys {
		if _, ok := s.byKey[key]; ok {
			delete(s.byKey, key)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}
	return true, s.saveLocked(d)
}

// find 按任意一侧的 ID 查绑定。这是双向共享的入口：
// 传官方 ID 得到旧身份，传旧 ID 得到官方身份，两者拿到的是同一条记录。
func (s *identityBindStore) find(d *Dice, id string) (*identityBindRecord, bool) {
	if strings.TrimSpace(id) == "" {
		return nil, false
	}
	s.ensureLoaded(d)
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.byKey[id]
	if !ok || record == nil {
		return nil, false
	}
	copied := *record
	return &copied, true
}

// lookupRecord 按任意一侧的 ID 查绑定记录，并确认记录类型匹配。
func (s *identityBindStore) lookupRecord(d *Dice, id string, action identityBindAction) (*identityBindRecord, bool) {
	record, ok := s.find(d, id)
	if !ok || record.Action != action {
		return nil, false
	}
	return record, true
}

// lookupUser 查用户绑定，返回**旧 QQ 号**。
//
// 无论传进来的是官方 ID 还是旧 QQ 号，返回的都是旧 QQ 号——
// 也就是「数据应该挂在哪个 key 上」的唯一答案。
func (s *identityBindStore) lookupUser(d *Dice, userID string) (string, bool) {
	record, ok := s.lookupRecord(d, userID, identityBindActionUser)
	if !ok {
		return "", false
	}
	oldID := strings.TrimSpace(record.Old.UserID)
	if oldID == "" {
		return "", false
	}
	return oldID, true
}

// lookupGroup 查群绑定，返回**旧群 ID**。
func (s *identityBindStore) lookupGroup(d *Dice, groupID string) (string, bool) {
	record, ok := s.lookupRecord(d, groupID, identityBindActionGroup)
	if !ok {
		return "", false
	}
	oldID := strings.TrimSpace(record.Old.GroupID)
	if oldID == "" {
		return "", false
	}
	return oldID, true
}

// list 返回所有绑定（已按记录去重，因为一条绑定有两个索引键），用于 .bind list 展示。
func (s *identityBindStore) list(d *Dice) []*identityBindRecord {
	s.ensureLoaded(d)
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]*identityBindRecord, 0, len(s.byKey))
	seen := map[*identityBindRecord]bool{}
	for _, record := range s.byKey {
		if record == nil || seen[record] {
			continue
		}
		seen[record] = true
		copied := *record
		records = append(records, &copied)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Created < records[j].Created })
	return records
}

// ---------- 解析（双向身份归一） ----------
//
// 这一层是「双向共享」的核心：不管消息来自官方 bot 还是民间 bot，只要这个身份参与过
// 绑定，就都会被归一成同一对 (群ID, 用户ID)，于是两边读写的永远是同一份数据。
//
// 注意：归一化只用于「数据寻址」。权限、昵称、@ 目标、回复对象一律仍用真实身份，
// 否则会出现「在官方群是普通玩家，绑到旧群管理员号就拿到了管理员权限」的提权漏洞。

// identityCanonicalUserID 把任意一侧的用户 ID 归一成数据层应使用的 ID。
//
// 归一的目标固定是**旧身份**（迁移前的 QQ 号）。这一点很关键：
//   - 旧号那一侧本来就什么都不用改，历史数据原地不动、继续可用；
//   - 官方那一侧改到旧号的名下，于是「两边算出来的 key 完全一样」。
//
// 如果反过来把官方 ID 当规范，旧账号的全部历史数据（属性、角色卡、日志）
// 都会瞬间读不到，正是这个功能要避免的事。
//
// 无绑定时原样返回。
func identityCanonicalUserID(d *Dice, userID string) string {
	if d == nil || strings.TrimSpace(userID) == "" {
		return userID
	}
	oldID, ok := identityBindStoreOf(d).lookupUser(d, userID)
	if !ok || oldID == "" {
		return userID
	}
	return oldID
}

// identityCanonicalGroupID 把任意一侧的群 ID 归一成数据层应使用的 ID（同样是旧群）。
// 无绑定时原样返回。
func identityCanonicalGroupID(d *Dice, groupID string) string {
	if d == nil || strings.TrimSpace(groupID) == "" {
		return groupID
	}
	oldID, ok := identityBindStoreOf(d).lookupGroup(d, groupID)
	if !ok || oldID == "" {
		return groupID
	}
	return oldID
}

// identityBindResolveGroup 解析当前群应该读取哪一份数据。
//
// 与旧实现的关键差别：
//   - 不再要求当前端点是官方 QQ，旧号侧同样能查到（这是双向共享的前提）；
//   - 归一目标固定是旧群，所以「官方群 → 旧群」和「旧群 → 旧群（原地不动）」都成立。
func identityBindResolveGroup(ctx *MsgContext) (*GroupInfo, bool) {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || ctx.Session == nil {
		return nil, false
	}
	targetID := identityCanonicalGroupID(ctx.Dice, ctx.Group.GroupID)
	if targetID == "" || targetID == ctx.Group.GroupID {
		return nil, false
	}
	// 旧群/目标群可能已经不在内存里（机器人没再访问过），此时不能凭空造一个，
	// 否则会读不到数据还报错，静默回退到当前群。
	target, exists := ctx.Session.ServiceAtNew.Load(targetID)
	if !exists || target == nil {
		return nil, false
	}
	return target, true
}

// identityBindResolveUserID 解析当前用户应该使用哪个 ID 去读写数据。
//
// 官方身份会解析成旧 QQ 号；旧身份解析结果就是它自己（返回 bound=false）。
func identityBindResolveUserID(ctx *MsgContext) (string, bool) {
	if ctx == nil || ctx.Dice == nil || ctx.Player == nil {
		return "", false
	}
	targetID := identityCanonicalUserID(ctx.Dice, ctx.Player.UserID)
	if targetID == "" || targetID == ctx.Player.UserID {
		return "", false
	}
	return targetID, true
}

// identityBindReadGroup 返回用于读取历史数据的群对象，以及是否命中绑定。
func identityBindReadGroup(ctx *MsgContext) (*GroupInfo, bool) {
	group, bound := identityBindResolveGroup(ctx)
	if bound && group != nil {
		return group, true
	}
	if ctx == nil {
		return nil, false
	}
	return ctx.Group, false
}

// identityBindReadPlayer 返回用于「读取」历史数据的玩家对象。
// 权限、昵称展示等仍然使用 ctx.Player（真实身份）。
func identityBindReadPlayer(ctx *MsgContext) *GroupPlayerInfo {
	if ctx == nil || ctx.Dice == nil {
		return nil
	}
	targetUserID, bound := identityBindResolveUserID(ctx)
	if !bound {
		return ctx.Player
	}
	readGroup, _ := identityBindReadGroup(ctx)
	if readGroup == nil {
		return ctx.Player
	}
	player := readGroup.PlayerGet(ctx.Dice.DBOperator, targetUserID)
	if player == nil {
		// 目标身份在本群还没有记录时，不能伪造数据，回退到当前玩家。
		return ctx.Player
	}
	return player
}

// identityBindDataUserID 取数据层用户 ID。
//
// 优先用 GetPlayerInfoBySenderRaw 预先填好的 ctx.DataUserID（那条路径覆盖所有平台）；
// 如果它被代骰之类的逻辑改过（与 ctx.Player.UserID 不一致），则以当前的 ctx.Player
// 重新解析一次，避免「换了玩家对象但 DataUserID 还是旧值」这种错配。
func identityBindDataUserID(ctx *MsgContext) string {
	if ctx == nil || ctx.Player == nil {
		return ""
	}
	// 必须先判 DataUserID 非空，再比较：空串与空串相等，顺序写反会导致
	// 未填写的 ctx 直接返回空字符串，静默丢掉所有数据寻址。
	if ctx.DataUserID != "" && ctx.DataUserID == identityCanonicalUserID(ctx.Dice, ctx.Player.UserID) {
		return ctx.DataUserID
	}
	return identityCanonicalUserID(ctx.Dice, ctx.Player.UserID)
}

// identityBindDataGroupID 取数据层群 ID，解析不出时回退到真实群 ID。
func identityBindDataGroupID(ctx *MsgContext) string {
	if ctx == nil || ctx.Group == nil {
		return ""
	}
	if ctx.DataGroupID != "" && ctx.DataGroupID == identityCanonicalGroupID(ctx.Dice, ctx.Group.GroupID) {
		return ctx.DataGroupID
	}
	return identityCanonicalGroupID(ctx.Dice, ctx.Group.GroupID)
}

// identityBindPlayerNameTemplate 取当前应该生效的 .sn 名片模板。
//
// 采用「只读回退」策略（方案 A）：自己这份模板为空时，才去绑定另一侧取；
// 写入永远写在使用者自己的身份上。这样官方群的 .sn 不会去改旧群的群名片
// （旧群的 .sn 是会真的调用平台接口改群名片的，两边语义不同，不该互相影响）。
func identityBindPlayerNameTemplate(ctx *MsgContext) string {
	if ctx == nil || ctx.Player == nil {
		return ""
	}
	if tmpl := strings.TrimSpace(ctx.Player.AutoSetNameTemplate); tmpl != "" {
		return tmpl
	}
	if ctx.Dice == nil {
		return ""
	}
	// 自身没有模板，看绑定另一侧有没有。
	if player := identityBindReadPlayer(ctx); player != nil && player != ctx.Player {
		if tmpl := strings.TrimSpace(player.AutoSetNameTemplate); tmpl != "" {
			return tmpl
		}
	}
	// 兜底：绑定的目标群当前不在内存里（机器人很久没去过）时，
	// 上面那条路径取不到玩家记录，这里在各群里按目标 ID 再找一次。
	targetUserID, bound := identityBindResolveUserID(ctx)
	if !bound {
		return ""
	}
	if player := identityBindFindPlayerAnyGroup(ctx, targetUserID); player != nil {
		return strings.TrimSpace(player.AutoSetNameTemplate)
	}
	return ""
}

// identityBindFindPlayerAnyGroup 在内存中的各个群里找某个用户的玩家记录，
// 只返回真正带 .sn 模板的那一个。用于「绑定的目标群当前不在内存里」时的兜底查询。
func identityBindFindPlayerAnyGroup(ctx *MsgContext, userID string) *GroupPlayerInfo {
	if ctx == nil || ctx.Dice == nil || ctx.Session == nil || userID == "" {
		return nil
	}
	var found *GroupPlayerInfo
	ctx.Session.ServiceAtNew.Range(func(_ string, group *GroupInfo) bool {
		if group == nil {
			return true
		}
		player := group.PlayerGet(ctx.Dice.DBOperator, userID)
		if player == nil {
			return true
		}
		if strings.TrimSpace(player.AutoSetNameTemplate) != "" {
			found = player
			return false
		}
		return true
	})
	return found
}

// ---------- 会话与冷却 ----------

func identityBindSessionKey(epID, userID string, action identityBindAction) string {
	return epID + "|" + userID + "|" + string(action)
}

func identityBindClearSession(key string) {
	globalIdentityBindSessions.Delete(key)
}

// identityBindCancelCode 清掉某个目标的验证码挑战。
// 返回 true 表示确实有一条待确认的挑战被取消。
func identityBindCancelCode(action identityBindAction, targetID string) bool {
	if strings.TrimSpace(targetID) == "" {
		return false
	}
	key := identityBindCodeKey(action, targetID)
	if _, ok := globalIdentityBindCodes.Load(key); !ok {
		return false
	}
	globalIdentityBindCodes.Delete(key)
	return true
}

// identityBindCooldownRemaining 返回还需要等待多久才能再次发起绑定。
func identityBindCooldownRemaining(d *Dice, epID, userID string, action identityBindAction) time.Duration {
	cooldown := identityBindCooldown(d)
	if cooldown > 0 {
		if value, ok := globalIdentityBindLastAttempt.Load(identityBindSessionKey(epID, userID, action)); ok && value != nil {
			elapsed := time.Since(time.Unix(value.At, 0))
			if elapsed < cooldown {
				return cooldown - elapsed
			}
		}
	}

	return 0
}

func identityBindMarkAttempt(epID, userID string, action identityBindAction) {
	globalIdentityBindLastAttempt.Store(
		identityBindSessionKey(epID, userID, action),
		&identityBindLastAttempt{At: time.Now().Unix()},
	)
}

// ---------- .bind 指令 ----------

func identityBindUserHelp() string {
	return `.bind <旧QQ号> [旧群号] // 绑定你的个人身份，之后你的角色卡/属性与旧 QQ 号共用同一份
.bind cancel // 取消进行中的绑定验证
.bind reset // 同上，顺便清掉群绑定的验证
.bind status // 查看自己的绑定
.bind list // 查看所有绑定记录，需要管理权限
.bind doctor // 自检所有绑定，需要管理权限
.unbind // 解除自己的个人绑定

【骰主专用】
.bind pending // 列出所有待处理的绑定申请
.bind approve <旧QQ号> // 人工确认一条申请（跳过验证码）

【怎么验证身份】
发起绑定后，会把一个验证码送到"只有那个旧 QQ 号的主人才能拿到"的地方，
两条通道自动二选一（都可用时按骰主的偏好设置）：
  · 邮箱通道：寄到 <旧QQ号>@qq.com，收到后**在官方 bot 这边**回复验证码；
  · 私聊通道：民间 bot（OneBot 连接）私聊发给旧 QQ 号，用那个号回复民间 bot。
只有真正持有那个旧 QQ 号的人能拿到验证码，所以别人抢不走你的绑定。

如果两条通道都发不出去（民间 bot 不在线、邮箱也没配），申请会转成人工：
骰主会收到私聊通知，可以用 .bind approve <旧QQ号> 核实后确认。

个人绑定与群无关、全局生效：在任何官方群里绑定一次即可。
旧群号只是可选参数（.bind <旧QQ号> <旧群号>），填了也只会用于展示。

注意：个人绑定只管「用户维度」。要让整个群的数据（含日志）也共通，
还需要再做一次群绑定：

.group bind <旧群号> // 发起群绑定，需要管理权限
.group pending // 列出待处理申请（骰主）
.group approve <旧群号> // 人工确认（骰主）
.group unbind
.group status
.group doctor // 自检所有绑定
.group cancel`
}

// runIdentityBindCommand 处理 .bind / .unbind。
func runIdentityBindCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil {
		return solved
	}
	d := ctx.Dice

	if cmdArgs.IsArgEqual(1, "help") {
		ReplyToSender(ctx, msg, identityBindUserHelp())
		return solved
	}

	action := identityBindActionUser
	// 正常解析后 Args[0] 是子指令本身（unbind / 旧QQ号）。
	// 注意 GetArgN(0) 在 Args 为空时会越界，必须自己判断。
	sub := ""
	if len(cmdArgs.Args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(cmdArgs.Args[0]))
	}

	// .unbind 不需要功能开关，否则用户开不了也解不了。
	if sub == "unbind" {
		return identityBindRunUnbind(ctx, msg, action)
	}

	// 骰主人工兜底：待确认列表 + 手动确认。
	// 刻意放在"仅官方 bot"检查之前——申请常常正是因为"民间 bot 不在线"
	// 才卡住的，骰主很可能就是在民间 bot 那边看到通知、顺手处理。
	switch sub {
	case "pending", "待确认":
		if ctx.PrivilegeLevel < 100 {
			ReplyToSender(ctx, msg, "待确认列表需要 master 权限。")
			return solved
		}
		ReplyToSender(ctx, msg, identityBindFormatPendingList(d, false))
		return solved
	case "approve", "pass":
		// 目标在 Args[1]，而 GetArgN 是 1-based（GetArgN(2) == Args[1]）
		return identityBindRunApprove(ctx, msg, cmdArgs.GetArgN(2))
	}

	if !identityBindEnabled(d) {
		ReplyToSender(ctx, msg, "身份绑定功能未开启，请让骰主在 serve.yaml 中把 identityBindEnable 设为 true。")
		return solved
	}
	if !identityBindSupported(ctx.EndPoint) {
		ReplyToSender(ctx, msg, "身份绑定仅用于 QQ 官方机器人，当前连接方式无需绑定。")
		return solved
	}

	// 取消 / 重置：同时清掉个人与群两种问答会话，避免卡在答题环节
	switch sub {
	case "cancel", "reset":
		return identityBindCancelSession(ctx, msg, true)
	}

	sessionKey := identityBindSessionKey(ctx.EndPoint.ID, ctx.Player.UserID, action)

	if cmdArgs.IsArgEqual(1, "status") {
		ReplyToSender(ctx, msg, identityBindFormatUserStatus(d, ctx))
		return solved
	}

	if cmdArgs.IsArgEqual(1, "list") {
		if ctx.PrivilegeLevel < 50 {
			ReplyToSender(ctx, msg, "你不具备管理权限")
			return solved
		}
		ReplyToSender(ctx, msg, identityBindFormatList(d))
		return solved
	}

	if cmdArgs.IsArgEqual(1, "doctor") || cmdArgs.IsArgEqual(1, "检查") {
		if ctx.PrivilegeLevel < 50 {
			ReplyToSender(ctx, msg, "你不具备管理权限")
			return solved
		}
		ReplyToSender(ctx, msg, identityBindDoctorReport(d, ctx))
		return solved
	}

	// 现在个人绑定也只走验证码，没有"答答案"这一步了：
	// 验证码由 identityBindTryConsumeCode 在消息分发层拦截。

	// 没有参数：输出帮助 + 当前状态，方便自查
	if len(cmdArgs.Args) == 0 {
		ReplyToSender(ctx, msg, identityBindUserHelp()+"\n\n"+identityBindFormatUserStatus(d, ctx))
		return solved
	}

	// 全新发起绑定：.bind <旧QQ号> [旧群号]（旧群号可选，个人绑定与群无关）
	oldUserID, err := normalizeIdentityBindUser(cmdArgs.Args[0])
	if err != nil {
		ReplyToSender(ctx, msg, err.Error())
		return solved
	}

	return identityBindStartUserSession(ctx, msg, cmdArgs, sessionKey, oldUserID)
}

// identityBindCancelSession 取消进行中的绑定（清掉已登记的验证码挑战）。
// clearAll 为 true 时同时清掉个人与群两种。
func identityBindCancelSession(ctx *MsgContext, msg *Message, clearAll bool) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.EndPoint == nil || ctx.Player == nil {
		return solved
	}

	targets := []struct {
		action identityBindAction
		id     string
	}{
		{identityBindActionUser, ctx.Player.UserID},
	}
	if clearAll && ctx.Group != nil {
		targets = append(targets, struct {
			action identityBindAction
			id     string
		}{identityBindActionGroup, ctx.Group.GroupID})
	}

	cleared := 0
	for _, t := range targets {
		if identityBindCancelCode(t.action, t.id) {
			cleared++
		}
		identityBindClearSession(identityBindSessionKey(ctx.EndPoint.ID, ctx.Player.UserID, t.action))
	}

	if cleared == 0 {
		ReplyToSender(ctx, msg, "当前没有进行中的绑定。")
		return solved
	}
	ReplyToSender(ctx, msg, "已取消进行中的绑定验证。绑定记录本身没有被修改，可以随时重新发起。")
	return solved
}

// identityBindStartUserSession 发起个人身份绑定。
//
// 注意：个人身份绑定是**全局跨群通用**的——官方 QQ 的 MemberOpenID 已经做到跨群一致
// （见 formatDiceIDOfficialQQMemberOpenID，它刻意忽略 GroupOpenID），
// 绑定的目标是"这个人的旧 QQ 号"，与旧群无关。角色卡是按 owner_id（旧 QQ 号）
// 查询的，所以不需要旧群号。旧群号只是可选参数，用来展示更友好的提示。
func identityBindStartUserSession(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs, sessionKey, oldUserID string) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if remaining := identityBindCooldownRemaining(d, ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionUser); remaining > 0 {
		ReplyToSender(ctx, msg, fmt.Sprintf("操作过于频繁，请在 %d 秒后重试。", int(remaining.Seconds())+1))
		return solved
	}

	// 双向索引之后，同一个旧 QQ 号只能属于一个人。
	// 不加这个检查的话，后绑定的人会把先绑定的人的数据「抢」过去。
	if existing, ok := identityBindStoreOf(d).find(d, oldUserID); ok && existing.Action == identityBindActionUser {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"该旧账号 %s 已经被绑定过了，无法重复绑定。\n如果你认为这是误绑，请联系管理员处理。", oldUserID))
		return solved
	}

	if existing, ok := identityBindStoreOf(d).find(d, ctx.Player.UserID); ok {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"你已经绑定到 %s 了，如需更换请先发送 `.unbind`。", existing.Old.UserID))
		return solved
	}

	// 旧群号是可选的：角色卡按 owner_id 查询，与群无关
	oldGroupID := ""
	if cmdArgs != nil {
		if rawGroup := strings.TrimSpace(cmdArgs.GetArgN(2)); rawGroup != "" {
			normalized, err := normalizeIdentityBindGroup(rawGroup)
			if err != nil {
				ReplyToSender(ctx, msg, err.Error())
				return solved
			}
			oldGroupID = normalized
		}
	}

	oldGroupName := ""
	oldPlayerName := ""
	if oldGroupID != "" {
		if oldGroup, ok := ctx.Session.ServiceAtNew.Load(oldGroupID); ok && oldGroup != nil {
			oldGroupName = oldGroup.GroupName
			if oldPlayer := oldGroup.PlayerGet(d.DBOperator, oldUserID); oldPlayer != nil {
				oldPlayerName = oldPlayer.Name
			}
		}
	}

	newEndpoint := identityBindEndpoint{
		Platform: ctx.EndPoint.Platform,
		Protocol: ctx.EndPoint.ProtocolType,
		GroupID:  ctx.Group.GroupID,
		UserID:   ctx.Player.UserID,
	}
	oldEndpoint := identityBindEndpoint{
		Platform:  "QQ",
		Protocol:  "onebot",
		GroupID:   oldGroupID,
		UserID:    oldUserID,
		GroupName: oldGroupName,
		UserName:  oldPlayerName,
	}

	// 统一走验证码。投递通道（私聊 / QQ 邮箱）由 identityBindStartCodeChallenge
	// 自动选择，不再有"答题"这条路。
	return identityBindStartCodeChallenge(ctx, msg, identityBindActionUser, newEndpoint, oldEndpoint, oldUserID)
}

// identityBindStartCodeChallenge 登记一条验证码挑战并告诉用户接下来会发生什么。
//
// confirmBy 是"只有谁能确认"：
//   - 个人绑定 = 被声明的旧 QQ 号（验证码私聊发给他）
//   - 群绑定   = 旧群的邀请人
func identityBindStartCodeChallenge(
	ctx *MsgContext, msg *Message,
	action identityBindAction,
	newEndpoint, oldEndpoint identityBindEndpoint,
	deliverTo string,
) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	code, err := identityBindGenerateCode(identityBindCodeLength(d))
	if err != nil {
		ReplyToSender(ctx, msg, "生成验证码失败，请稍后重试或联系骰主。")
		return solved
	}

	now := time.Now()
	challenge := &identityBindCodeChallenge{
		Action:    action,
		New:       newEndpoint,
		Old:       oldEndpoint,
		Code:      code,
		DeliverTo: deliverTo,
		ConfirmBy: deliverTo,
		Status:    identityBindCodePending,
		CreatedAt: now.Unix(),
		ExpiresAt: now.Add(identityBindCodeExpiry(d)).Unix(),
	}
	identityBindPutCode(challenge)
	identityBindMarkAttempt(ctx.EndPoint.ID, ctx.Player.UserID, action)

	// 立刻尝试投递一次，省掉最多一个 tick 的等待
	identityBindDeliverPendingCodes(d)

	// 投递是否成功要在投递之后再读一次状态（按发起者取回同一条挑战）
	latest, _ := identityBindLoadCode(action, identityBindChallengeOwnerID(action, newEndpoint))
	delivered := latest != nil && latest.Status == identityBindCodeDelivered
	reason := ""
	if latest != nil {
		reason = latest.Reason
	}
	needMaster := latest != nil && latest.NeedMaster

	// 骰主手动确认的写法（用于给骰主的提示 / 给普通用户的转告话术）
	approveHint := fmt.Sprintf("`.bind approve %s`", identityBindBareNumber(oldEndpoint.UserID))
	if action == identityBindActionGroup {
		approveHint = fmt.Sprintf("`.group bindforce %s`", identityBindBareNumber(oldEndpoint.GroupID))
	}

	ttl := identityBindFormatDuration(identityBindCodeExpiry(d))
	var lines []string
	switch {
	case delivered && latest.Channel == identityBindCodeChannelEmail:
		// 邮箱通道：码进了邮箱，回复地点是**官方 bot 这边**（不是民间 bot）
		who := "这个 QQ 号"
		if action == identityBindActionGroup {
			who = "旧群邀请人"
		}
		lines = append(lines,
			fmt.Sprintf("验证码已寄到%s（QQ %s）的 QQ 邮箱：%s",
				who, identityBindMailTargetQQ(latest), latest.SentTo),
			"拿到验证码后**在官方 bot 这边**把它回复过来即可完成绑定——发在群里或私聊都行。",
			"没收到的话记得翻一下垃圾邮件。",
		)
	case delivered:
		lines = append(lines,
			fmt.Sprintf("已通过民间 bot 给 %s 发送了私聊验证码。", deliverTo),
			"请**用那个号**打开与民间 bot 的私聊，把收到的验证码回复过去即可完成绑定。",
		)
	case needMaster:
		// 两条通道都发不出去：已经转人工，这里必须说清楚"谁在等、等谁"
		lines = append(lines, "这条申请已经登记，但验证码两条通道都发不出去。")
		if len(latest.MasterNotifiedTo) > 0 {
			lines = append(lines, fmt.Sprintf("已私聊通知骰主（%s），请等骰主核实身份后确认。",
				strings.Join(latest.MasterNotifiedTo, "、")))
		} else {
			lines = append(lines, "而且**没能通知到骰主**——民间 bot 不在线时，官方 bot 无法按 QQ 号主动私聊。")
		}
		if ctx.PrivilegeLevel >= 100 {
			lines = append(lines, fmt.Sprintf(
				"你就是骰主：核实对方身份后可以直接用 %s 确认（会跳过验证码）。", approveHint))
		} else {
			lines = append(lines, fmt.Sprintf(
				"请把这条申请转告骰主，让骰主用 %s 确认。", approveHint))
		}
	case action == identityBindActionGroup:
		lines = append(lines,
			fmt.Sprintf("已登记确认请求，验证码会私聊发给旧群的邀请人 %s。", deliverTo),
		)
	default:
		lines = append(lines,
			fmt.Sprintf("已登记验证请求，验证码会私聊发到 %s。", deliverTo),
		)
	}
	if reason != "" {
		lines = append(lines, "", "⚠️ "+reason)
		if !needMaster {
			lines = append(lines, "请确认民间 bot（OneBot 连接）在线且已启用；修好后稍等片刻会自动重试。")
		}
	}
	if needMaster {
		lines = append(lines, "", "这条申请会保留 12 小时，骰主确认后立即生效。")
	} else {
		lines = append(lines, "", fmt.Sprintf("验证码 %s 内有效。", ttl))
	}

	ReplyToSender(ctx, msg, strings.Join(lines, "\n"))
	return solved
}

// identityBindFormatUserStatus 生成个人绑定状态。
// 只展示「个人身份绑定」的信息；群绑定请用 .group status 查看，两者是独立的。
func identityBindFormatUserStatus(d *Dice, ctx *MsgContext) string {
	lines := []string{
		"【个人身份绑定】把你自己指向迁移前的旧 QQ 号。绑定后官方身份与旧 QQ 号共用同一套数据（角色卡/属性）。",
		"作用范围: 用户维度，全局（与群无关，绑定一次所有官方群通用）",
		fmt.Sprintf("当前身份: %s", ctx.Player.UserID),
	}
	if record, ok := identityBindStoreOf(d).lookupRecord(d, ctx.Player.UserID, identityBindActionUser); ok {
		lines = append(lines, fmt.Sprintf("已绑定旧QQ号: %s", record.Old.UserID))
		if record.Old.UserName != "" {
			lines = append(lines, fmt.Sprintf("旧群内昵称: %s", record.Old.UserName))
		}
		if record.Old.GroupID != "" {
			lines = append(lines, fmt.Sprintf("绑定时填写的旧群: %s", record.Old.GroupID))
		}
		lines = append(lines, fmt.Sprintf("数据存放位置: %s（旧号名下，数据未做任何搬移）", record.Old.UserID))
		lines = append(lines, fmt.Sprintf("绑定时间: %s", time.Unix(record.Created, 0).Format("2006-01-02 15:04")))
		lines = append(lines, "解除方式: .unbind")
	} else {
		lines = append(lines, "尚未绑定旧QQ号")
		lines = append(lines, "发起绑定: .bind <旧QQ号>")
	}
	// 顺带提示本群的群绑定状态，避免两个功能混淆。
	// 这里要说清「两个维度」：个人绑定管用户，群绑定管群，两者都做才会完全共用同一份数据。
	if ctx.Group != nil {
		if record, ok := identityBindStoreOf(d).lookupRecord(d, ctx.Group.GroupID, identityBindActionGroup); ok {
			lines = append(lines, fmt.Sprintf("（本群群绑定: 旧群 %s，群维度已打通）", record.Old.GroupID))
		} else {
			lines = append(lines, "（本群未做群绑定：用户维度已打通，但群维度没有，因此本群仍读本群自己的数据）")
			lines = append(lines, "（要让本群与旧群完全共用同一份数据，请另外执行 .group bind <旧群号>）")
		}
	}
	return strings.Join(lines, "\n")
}

func identityBindFormatList(d *Dice) string {
	records := identityBindStoreOf(d).list(d)
	if len(records) == 0 {
		return "当前没有任何绑定记录。"
	}
	lines := []string{fmt.Sprintf("共 %d 条绑定记录：", len(records))}
	for _, record := range records {
		kind := "身份"
		if record.Action == identityBindActionGroup {
			kind = "日志"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s → %s (%s)",
			kind, identityBindRecordEndpointID(record), identityBindRecordOldID(record),
			time.Unix(record.Created, 0).Format("2006-01-02 15:04"),
		))
	}
	return strings.Join(lines, "\n")
}

// identityBindDoctorReport 生成绑定自检报告。
//
// 为什么需要它：绑定失败时绝大多数情况是「静默回退」——比如绑定的目标群
// 已经不在内存里（机器人很久没去过那个群），此时读取会悄悄退回当前群，
// 表现就是「绑定成功了但数据没变」，从表面完全看不出原因。
// 这个报告把这些隐式条件显式列出来。
func identityBindDoctorReport(d *Dice, ctx *MsgContext) string {
	store := identityBindStoreOf(d)
	records := store.list(d)

	lines := []string{
		"【绑定自检】",
		fmt.Sprintf("绑定总开关: %s", identityBindBoolText(identityBindEnabled(d))),
		fmt.Sprintf("绑定记录数: %d", len(records)),
	}
	if ctx != nil && ctx.Group != nil {
		lines = append(lines, fmt.Sprintf("当前群: %s", ctx.Group.GroupID))
	}

	if len(records) == 0 {
		lines = append(lines, "没有任何绑定记录，无需检查。")
		return strings.Join(lines, "\n")
	}

	userCount, groupCount, problemCount := 0, 0, 0
	for _, record := range records {
		switch record.Action {
		case identityBindActionUser:
			userCount++
		case identityBindActionGroup:
			groupCount++
		}

		kind := "身份"
		newID := identityBindRecordEndpointID(record)
		oldID := identityBindRecordOldID(record)
		if record.Action == identityBindActionGroup {
			kind = "群"
		}

		var issues []string
		if newID == "" {
			issues = append(issues, "缺少官方侧 ID（记录损坏）")
		}
		if oldID == "" {
			issues = append(issues, "缺少旧侧 ID（记录损坏）")
		}
		// 关键检查：目标群是否在内存里。不在内存里时读取会静默回退。
		if record.Action == identityBindActionGroup && oldID != "" {
			if ctx == nil || ctx.Session == nil {
				issues = append(issues, "无法检查目标群（没有会话上下文）")
			} else if _, ok := ctx.Session.ServiceAtNew.Load(oldID); !ok {
				issues = append(issues, "目标旧群不在内存里，读取会静默回退到当前群（让骰子去那个群发一条消息即可恢复）")
			}
		}

		head := fmt.Sprintf("%s绑定: %s → %s", kind, newID, oldID)
		if len(issues) == 0 {
			lines = append(lines, head+"  [正常]")
			continue
		}
		problemCount++
		lines = append(lines, head+"  [!]")
		for _, issue := range issues {
			lines = append(lines, "    - "+issue)
		}
	}

	lines = append(lines,
		"",
		fmt.Sprintf("统计: 个人绑定 %d 条，群绑定 %d 条，异常 %d 条", userCount, groupCount, problemCount),
	)
	if problemCount == 0 {
		lines = append(lines, "结论: 所有绑定看起来都正常。")
	} else {
		lines = append(lines, "结论: 存在异常项，请按上面的提示处理。")
	}
	lines = append(lines,
		"",
		"提醒: 完整的双向共通需要「个人绑定 + 群绑定」同时生效——",
		"个人绑定负责用户维度，群绑定负责群维度，缺一个就会有一边读不到数据。",
	)

	// 单边绑定对「将来回退到官方主线」是有代价的，而这件事必须在**回退之前**说，
	// 回退之后再说就晚了。所以这里主动提示一次。
	if userCount > 0 && groupCount == 0 {
		lines = append(lines,
			"",
			"⚠️ 只做了个人绑定、没有群绑定：",
			"  你在官方群里改动/新建的角色卡属性，key 会落在「官方群-旧号」上。",
			"  将来若换回不带绑定功能的官方主线，那里按「旧群-旧号」查，会读不到这些改动。",
			"  想避免的话，现在就补一次群绑定：.group bind <旧群号>",
		)
	} else if groupCount > 0 && userCount == 0 {
		lines = append(lines,
			"",
			"⚠️ 只做了群绑定、没有个人绑定：",
			"  日志是共通的，但玩家属性还未归一，将来回退到官方主线可能读不到。",
			"  请让玩家各自执行一次 .bind <旧QQ号>。",
		)
	}
	return strings.Join(lines, "\n")
}

func identityBindBoolText(v bool) string {
	if v {
		return "已开启"
	}
	return "已关闭"
}

// identityBindRunUnbind 处理 .unbind。
func identityBindRunUnbind(ctx *MsgContext, msg *Message, action identityBindAction) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if !identityBindSupported(ctx.EndPoint) {
		ReplyToSender(ctx, msg, "身份绑定仅用于 QQ 官方机器人，当前连接方式无需解绑。")
		return solved
	}

	// 用当前身份查绑定；群绑定看群，个人绑定看用户。
	lookupID := ctx.Player.UserID
	if action == identityBindActionGroup {
		lookupID = ctx.Group.GroupID
	}
	record, ok := identityBindStoreOf(d).find(d, lookupID)
	if !ok || record.Action != action {
		ReplyToSender(ctx, msg, "没有找到属于你的绑定记录。")
		return solved
	}
	deleted, err := identityBindStoreOf(d).delete(d, record)
	if err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("解除绑定失败: %v", err))
		return solved
	}
	if !deleted {
		ReplyToSender(ctx, msg, "没有找到属于你的绑定记录。")
		return solved
	}
	ReplyToSender(ctx, msg, "已解除绑定，之后将重新使用当前官方机器人身份的数据。")
	ctx.Notice(fmt.Sprintf(
		"身份绑定已解除: 群 <%s>(%s) 用户 <%s>(%s)",
		ctx.Group.GroupName, ctx.Group.GroupID, msg.Sender.Nickname, ctx.Player.UserID,
	), NoticeTypeGroup)
	d.LastUpdatedTime = time.Now().Unix()
	return solved
}

// ---------- .log bind / .log unbind ----------

func identityBindLogHelp() string {
	return `【群绑定】把当前官方群指向迁移前的旧群。绑定后本群与旧群共用同一份群维度数据：
日志状态（.log on / new / off）与日志内容都记在同一份记录里，两边都能读到。

.group bind <旧群号> // 发起群绑定（需要管理权限）
.group pending // 列出待处理的绑定申请（仅 master）
.group approve <旧群号> // 骰主人工确认，跳过验证码（仅 master）
.group bindforce <旧群号> // 同上，等价写法；区别是它绑"骰主当前所在的群"
.group cancel // 取消进行中的绑定验证（同时清掉个人绑定的验证）
.group unbind // 解除当前群的绑定（需要管理权限）
.group status // 查看当前群的绑定
.group doctor // 自检所有绑定（需要管理权限）

【怎么验证】
发起后会把验证码送到旧群的**邀请人**（把骰子拉进旧群的那个人）手里，
两条通道自动二选一：
  · 邮箱通道：寄到 邀请人QQ号@qq.com，拿到码后**在官方 bot 这边**回复即可；
  · 私聊通道：民间 bot 私聊发给邀请人，由他在民间 bot 那边回复。
回复的人不必是发起绑定的人：码只有拿到邮件/私聊的人才有，
所以群管理拿到邀请人转发的码同样可以确认。

如果两条通道都不通（没有民间 bot、邀请人也没开 QQ 邮箱），
申请会自动转人工并私聊通知骰主，骰主用 .group approve <旧群号> 确认
——**请先自行核实对方身份**。

说明：
* 群绑定负责「群维度」，个人身份绑定（.bind）负责「用户维度」。
  两个都做，官方身份与旧号才会完全共用同一套数据；只做一个会有半边读不到。
* 数据不做任何搬移，规范位置固定在旧群 / 旧号名下，.unbind 后立刻恢复原状。
* 兼容写法：.log bind / .log unbind / .log bindstatus 与上面等价。`
}

// identityBindGroupStatus 生成当前群的绑定状态。
// 只展示「群绑定」的信息；个人身份绑定请用 .bind status 查看，两者是独立的。
func identityBindGroupStatus(d *Dice, ctx *MsgContext) string {
	lines := []string{
		"【群绑定】把整个群指向迁移前的旧群。绑定后本群与旧群共用同一份群维度数据（日志、群配置）。",
		fmt.Sprintf("当前群: %s", ctx.Group.GroupID),
	}
	if record, ok := identityBindStoreOf(d).lookupRecord(d, ctx.Group.GroupID, identityBindActionGroup); ok {
		lines = append(lines, fmt.Sprintf("已绑定旧群: %s", record.Old.GroupID))
		if record.Old.GroupName != "" {
			lines = append(lines, fmt.Sprintf("旧群名称: %s", record.Old.GroupName))
		}
		lines = append(lines, fmt.Sprintf("数据存放位置: %s（旧群名下，数据未做任何搬移）", record.Old.GroupID))
		lines = append(lines, fmt.Sprintf("绑定时间: %s", time.Unix(record.Created, 0).Format("2006-01-02 15:04")))
		lines = append(lines, "解除方式: .group unbind")
	} else {
		lines = append(lines, "尚未绑定旧群")
		lines = append(lines, "发起绑定: .group bind <旧群号>")
	}
	// 说明两个维度的关系，避免「绑定成功了但数据没通」这种困惑。
	if ctx.Player != nil {
		if _, ok := identityBindStoreOf(d).lookupRecord(d, ctx.Player.UserID, identityBindActionUser); ok {
			lines = append(lines, "（你的个人身份绑定已生效：两个维度都打通了，本群与旧群完全共用同一份数据）")
		} else {
			lines = append(lines, "（群维度已打通，但你还没有做个人身份绑定：请再用 .bind <旧QQ号> 打通用户维度）")
		}
	}
	lines = append(lines, "（个人身份绑定与群绑定互相独立，用 .bind status 查看个人绑定）")
	return strings.Join(lines, "\n")
}

// runIdentityBindGroupCommand 处理全局指令 .group。
//
// 支持：
//
//	.group bind <旧群号>      // 发起群绑定（需要管理权限）
//	.group bindforce <旧群号> // 骰主手动确认，跳过验证码（需要 master）
//	.group unbind             // 解除当前群的绑定（需要管理权限）
//	.group status             // 查看当前群的绑定
//	.group doctor             // 自检所有绑定（需要管理权限）
//
// 群绑定与用户身份绑定（.bind）完全独立，互不影响。
func runIdentityBindGroupCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil {
		return solved
	}

	if cmdArgs.IsArgEqual(1, "help") {
		ReplyToSender(ctx, msg, identityBindLogHelp())
		return solved
	}

	sub := ""
	if len(cmdArgs.Args) > 0 {
		sub = strings.ToLower(strings.TrimSpace(cmdArgs.Args[0]))
	}

	// 骰主人工兜底：这两个子指令**不要求"当前在某个群里"**——
	// 骰主常常是在私聊里（甚至民间 bot 那边）看到通知后直接处理的。
	if sub == "pending" || sub == "待确认" {
		if ctx.PrivilegeLevel < 100 {
			ReplyToSender(ctx, msg, "待确认列表需要 master 权限。")
			return solved
		}
		ReplyToSender(ctx, msg, identityBindFormatPendingList(ctx.Dice, false))
		return solved
	}
	if sub == "approve" || sub == "pass" {
		// 目标在 Args[1]，而 GetArgN 是 1-based（GetArgN(2) == Args[1]）
		return identityBindRunApprove(ctx, msg, cmdArgs.GetArgN(2))
	}

	if ctx.Group == nil {
		return solved
	}

	// 取消 / 重置：群绑定和个人身份绑定的问答都会被清掉
	if sub == "cancel" || sub == "reset" {
		return identityBindCancelSession(ctx, msg, true)
	}

	// .group status 查看当前群绑定
	if sub == "status" || sub == "bindstatus" {
		ReplyToSender(ctx, msg, identityBindGroupStatus(ctx.Dice, ctx))
		return solved
	}

	// .group doctor 自检（需要管理权限）
	if sub == "doctor" || sub == "检查" {
		if ctx.PrivilegeLevel < 50 {
			ReplyToSender(ctx, msg, "你不具备管理权限")
			return solved
		}
		ReplyToSender(ctx, msg, identityBindDoctorReport(ctx.Dice, ctx))
		return solved
	}

	// .group bindforce <旧群号> 骰主手动确认（兜底）
	if sub == "bindforce" || sub == "force" {
		return identityBindRunForceBind(ctx, msg, cmdArgs)
	}

	// .group unbind 解除当前群绑定
	if sub == "unbind" {
		return identityBindRunUnbind(ctx, msg, identityBindActionGroup)
	}

	// .group bind ... 与 .log bind ... 走同一套实现
	return identityBindRunGroupBind(ctx, msg, cmdArgs)
}

// runIdentityBindLogCommand 处理 .log 下的 bind / unbind / bindstatus 子指令。
// 第二个返回值表示是否已经处理该子指令。
// 保留 .log 入口只是为了向后兼容，实际逻辑与 .group 共用一套。
func runIdentityBindLogCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) (CmdExecuteResult, bool) {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil {
		return solved, false
	}
	d := ctx.Dice

	sub := ""
	for i := 1; i <= 3; i++ {
		if arg := cmdArgs.GetArgN(i); arg != "" {
			sub = arg
			break
		}
	}
	switch sub {
	case "bind", "unbind", "bindstatus", "bindforce", "force":
	default:
		return solved, false
	}

	// .log bind help / .log unbind help 输出子指令帮助
	if cmdArgs.IsArgEqual(2, "help") {
		ReplyToSender(ctx, msg, identityBindLogHelp())
		return solved, true
	}

	if sub == "bindstatus" {
		ReplyToSender(ctx, msg, identityBindGroupStatus(d, ctx))
		return solved, true
	}

	// .log bindforce / .log force 也转发到手动确认，保持别名一致
	if sub == "bindforce" || sub == "force" {
		return identityBindRunForceBind(ctx, msg, cmdArgs), true
	}

	if sub == "unbind" {
		return identityBindRunUnbind(ctx, msg, identityBindActionGroup), true
	}

	return identityBindRunGroupBind(ctx, msg, cmdArgs), true
}

// identityBindRunGroupBind 群绑定（把当前官方群绑定到迁移前的旧群）的统一实现。
//
// 关键点：它与个人身份绑定（.bind）完全独立——
// 两者使用不同的存储键（group|群ID 与 user|用户ID），互不覆盖、互不阻断。
// 因此同一个群可以先做群绑定，群成员再各自做身份绑定，两个功能可以同时使用。
func identityBindRunGroupBind(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil {
		return solved
	}
	d := ctx.Dice
	action := identityBindActionGroup

	if !identityBindSupported(ctx.EndPoint) {
		ReplyToSender(ctx, msg, "群绑定仅用于 QQ 官方机器人，当前连接方式无需绑定。")
		return solved
	}
	if !identityBindEnabled(d) {
		ReplyToSender(ctx, msg, "身份与日志绑定功能未开启，请让骰主在 serve.yaml 中把 identityBindEnable 设为 true。")
		return solved
	}
	if ctx.PrivilegeLevel < 50 {
		ReplyToSender(ctx, msg, "群绑定需要管理权限（群主或管理员）。")
		return solved
	}

	// 统一取第一个参数作为子指令：.group bind / .group / .log bind 都适用
	sub := strings.ToLower(cmdArgs.GetArgN(1))
	if sub != "bind" {
		sub = ""
	}

	// 现在群绑定只走验证码，没有"答题会话"了。
	// 验证码由 identityBindTryConsumeCode 在消息分发层拦截，不会走到这里。

	// 解析旧群号：
	//   .group bind <旧群号> / .log bind <旧群号> -> 参数在 Args[1]
	//   .group <旧群号>                          -> 参数在 Args[0]
	rawGroup := cmdArgs.GetArgN(2)
	if sub != "bind" {
		rawGroup = sub
	}
	if rawGroup == "" {
		ReplyToSender(ctx, msg, "请使用 `.group bind <旧群号>` 的格式，例如 `.group bind 123456`。")
		return solved
	}
	oldGroupID, err := normalizeIdentityBindGroup(rawGroup)
	if err != nil {
		ReplyToSender(ctx, msg, err.Error())
		return solved
	}

	if remaining := identityBindCooldownRemaining(d, ctx.EndPoint.ID, ctx.Player.UserID, action); remaining > 0 {
		ReplyToSender(ctx, msg, fmt.Sprintf("操作过于频繁，请在 %d 秒后重试。", int(remaining.Seconds())+1))
		return solved
	}

	// 双向索引之后，同一个旧群也只能绑定一次。
	if existing, ok := identityBindStoreOf(d).find(d, oldGroupID); ok && existing.Action == identityBindActionGroup {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"旧群 %s 已经被绑定到另一个群了，无法重复绑定。", oldGroupID))
		return solved
	}

	if existing, ok := identityBindStoreOf(d).find(d, ctx.Group.GroupID); ok {
		ReplyToSender(ctx, msg, fmt.Sprintf("本群已经绑定到 %s 了，如需更换请先发送 `.group unbind`。", existing.Old.GroupID))
		return solved
	}

	newEndpoint := identityBindEndpoint{
		Platform: ctx.EndPoint.Platform,
		Protocol: ctx.EndPoint.ProtocolType,
		GroupID:  ctx.Group.GroupID,
		UserID:   ctx.Player.UserID,
	}
	oldGroupName := ""
	oldGroupObj, oldGroupInMemory := ctx.Session.ServiceAtNew.Load(oldGroupID)
	if oldGroupInMemory && oldGroupObj != nil {
		oldGroupName = oldGroupObj.GroupName
	}
	oldEndpoint := identityBindEndpoint{
		Platform:  "QQ",
		Protocol:  "onebot",
		GroupID:   oldGroupID,
		GroupName: oldGroupName,
	}

	// 群绑定的确认人是旧群的邀请人（把骰子拉进旧群的那个人）：
	// 他必然是旧群成员，所以由他确认能证明"这个旧群确实是我们见过的那个"。
	//
	// 投递通道两条都支持：
	//   · 私聊 → 直接发给邀请人本人
	//   · 邮箱 → 寄 邀请人QQ号@qq.com（连民间 bot 都不需要，适合纯官 bot 部署）
	if !oldGroupInMemory || oldGroupObj == nil {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"旧群 %s 目前不在骰子内存里，无法确定把验证码发给谁。\n"+
				"请先让骰子在旧群收到一条消息，然后再试一次。", oldGroupID))
		return solved
	}
	// 群绑定同样支持两条通道：私聊发给邀请人，或寄邀请人的 QQ 邮箱。
	// （纯官 bot 部署、没有民间 bot 时，邮箱是唯一出路。）
	confirmer := strings.TrimSpace(oldGroupObj.InviteUserID)
	if confirmer == "" {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"拿不到旧群 %s 的邀请人信息，无法确定把验证码发给谁。\n"+
				"可以：\n"+
				"  · 让骰子重新被拉进那个群（会记录邀请人），或\n"+
				"  · 骰主用 `.group bindforce %s` 手动确认（master 权限）",
			oldGroupID, oldGroupID))
		return solved
	}
	return identityBindStartCodeChallenge(ctx, msg, action, newEndpoint, oldEndpoint, confirmer)
}

// identityBindFormatPendingList 列出待处理的绑定申请（骰主排查用）。
func identityBindFormatPendingList(d *Dice, onlyNeedMaster bool) string {
	list := identityBindPendingChallenges(onlyNeedMaster)
	if len(list) == 0 {
		if onlyNeedMaster {
			return "当前没有等待骰主确认的申请。"
		}
		return "当前没有任何待处理的绑定申请。"
	}
	lines := []string{fmt.Sprintf("共 %d 条待处理申请（按发起时间排序）：", len(list))}
	for _, c := range list {
		lines = append(lines, identityBindDescribeChallenge(c))
		if c.Reason != "" {
			lines = append(lines, "    原因: "+c.Reason)
		}
	}
	lines = append(lines,
		"",
		"手动确认（需要 master 权限，会跳过验证码，请先核实对方身份）:",
		"  个人绑定: .bind approve <旧QQ号>",
		"  群绑定  : .group approve <旧群号>（等价于 .group bindforce <旧群号>）",
	)
	return strings.Join(lines, "\n")
}

// identityBindMatchPending 按用户输入的旧号 / 旧群找一条待确认申请。
//
// 裸数字会有歧义（旧 QQ 号与旧群号都可能是 6~10 位数字），所以：
// 带前缀的按前缀判定；裸数字先当个人绑定找，找不到再当群绑定找。
func identityBindMatchPending(raw string) *identityBindCodeChallenge {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil
	}
	switch {
	case strings.HasPrefix(value, identityBindGroupPrefix):
		c, _ := identityBindFindPendingChallenge(identityBindActionGroup, value)
		return c
	case strings.HasPrefix(value, identityBindUserPrefix):
		c, _ := identityBindFindPendingChallenge(identityBindActionUser, value)
		return c
	}
	if c, ok := identityBindFindPendingChallenge(identityBindActionUser, value); ok {
		return c
	}
	c, _ := identityBindFindPendingChallenge(identityBindActionGroup, value)
	return c
}

// identityBindRunApprove 骰主人工确认一条已经登记好的申请（跳过验证码）。
//
// 与 .group bindforce 的区别：
//   - bindforce 绑的是"骰主当前所在的那个群"；
//   - approve 绑的是"申请人登记在挑战里的那个身份"，所以骰主在哪里敲都行
//     （群里、私聊、甚至民间 bot 那边都能处理）。
//
// 触发场景很明确：民间 bot 不在线、邮箱又没配好，验证码两条通道都发不出去，
// 申请被转成人工。这时骰主是唯一的出口，所以这条指令必须好用、好记。
func identityBindRunApprove(ctx *MsgContext, msg *Message, rawTarget string) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil {
		return solved
	}
	d := ctx.Dice
	if !identityBindEnabled(d) {
		ReplyToSender(ctx, msg, "身份与日志绑定功能未开启，请让骰主在 serve.yaml 中把 identityBindEnable 设为 true。")
		return solved
	}
	if ctx.PrivilegeLevel < 100 {
		ReplyToSender(ctx, msg, "手动确认需要 master 权限（该操作会跳过验证码，等同骰主替对方作证）。")
		return solved
	}

	target := strings.TrimSpace(rawTarget)
	if target == "" {
		ReplyToSender(ctx, msg,
			"请指定要确认的旧号：\n"+
				"  · 个人绑定: `.bind approve <旧QQ号>`\n"+
				"  · 群绑定  : `.group approve <旧群号>`\n"+
				"用 `.bind pending` 可以查看当前的待确认申请。")
		return solved
	}

	challenge := identityBindMatchPending(target)
	if challenge == nil {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"没有找到与 %s 对应的待确认申请。\n用 `.bind pending` 查看当前有哪些申请。", target))
		return solved
	}

	// 唯一性检查：和正常绑定路径保持一致，不允许把同一个旧号 / 旧身份绑两次
	oldID := challenge.Old.UserID
	newID := challenge.New.UserID
	if challenge.Action == identityBindActionGroup {
		oldID = challenge.Old.GroupID
		newID = challenge.New.GroupID
	}
	var issues []string
	if existing, ok := identityBindStoreOf(d).find(d, oldID); ok {
		issues = append(issues, fmt.Sprintf("%s 已经被绑定到 %s 了，请先解除", oldID, identityBindRecordEndpointID(existing)))
	}
	if existing, ok := identityBindStoreOf(d).find(d, newID); ok {
		issues = append(issues, fmt.Sprintf("%s 已经绑定了 %s，请先解除", newID, identityBindRecordOldID(existing)))
	}
	if len(issues) > 0 {
		ReplyToSender(ctx, msg, "无法手动确认：\n  · "+strings.Join(issues, "\n  · "))
		return solved
	}

	if err := identityBindCommitChallenge(d, challenge); err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("手动确认失败: %v", err))
		return solved
	}

	// 群绑定成功后，真实群（官方群）上残留的日志状态要清掉，
	// 否则解绑或回退官方主线时会"复活"成一份同名空日志（与正常绑定路径一致）。
	if challenge.Action == identityBindActionGroup && ctx.Session != nil {
		if realGroup, ok := ctx.Session.ServiceAtNew.Load(challenge.New.GroupID); ok && realGroup != nil {
			identityBindResetRealGroupLogState(ctx, realGroup)
		}
	}

	identityBindNotifyNewSide(d, challenge)

	if challenge.Action == identityBindActionGroup {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"已手动确认：官方群 %s 与旧群 %s 绑定成功，双方共用同一份日志。\n"+
				"（本次跳过了验证码——请确认你确实核实过对方身份。）\n"+
				"如需解除请让对方在群里发送 `.group unbind`。",
			challenge.New.GroupID, challenge.Old.GroupID))
		ctx.Notice(fmt.Sprintf(
			"群绑定（骰主手动确认）: 群 %s 已绑定到旧群 %s，申请人 %s",
			challenge.New.GroupID, challenge.Old.GroupID, challenge.New.UserID,
		), NoticeTypeGroup)
	} else {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"已手动确认：%s 与旧 QQ 号 %s 绑定成功，双方共用同一份数据。\n"+
				"（本次跳过了验证码——请确认你确实核实过对方身份。）\n"+
				"如需解除请让对方发送 `.unbind`。",
			challenge.New.UserID, challenge.Old.UserID))
		ctx.Notice(fmt.Sprintf(
			"身份绑定（骰主手动确认）: 用户 %s 已绑定到旧 QQ 号 %s",
			challenge.New.UserID, challenge.Old.UserID,
		), NoticeTypeGroup)
	}
	d.LastUpdatedTime = time.Now().Unix()
	return solved
}

// identityBindRunForceBind 骰主手动确认绑定（跳过验证码）。
//
// 为什么需要这个兜底：验证码依赖两条通道，而现实中两条都可能不可用——
// 比如骰主已放弃民间 bot（私聊发不了），而旧群的邀请人没开通 QQ 邮箱
// （邮件退信）。此时群绑定就彻底卡死，没有一个能推进的口子。
//
// **只给 master**（`.group` 的其它子指令是 ≥50，这里是 100），
// 因为它本质上是"骰主替玩家作证"，属于最高信任级别的操作。
func identityBindRunForceBind(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil {
		return solved
	}
	d := ctx.Dice

	if !identityBindSupported(ctx.EndPoint) {
		ReplyToSender(ctx, msg, "该指令仅用于 QQ 官方机器人。")
		return solved
	}
	if !identityBindEnabled(d) {
		ReplyToSender(ctx, msg, "身份与日志绑定功能未开启，请让骰主在 serve.yaml 中把 identityBindEnable 设为 true。")
		return solved
	}
	if ctx.PrivilegeLevel < 100 {
		ReplyToSender(ctx, msg, "手动确认需要 master 权限（该操作会跳过验证码，等同骰主替对方作证）。")
		return solved
	}

	// 参数位置：.group bindforce <旧群号>
	rawGroup := cmdArgs.GetArgN(2)
	if strings.TrimSpace(rawGroup) == "" {
		// 兼容 .group force <旧群号> 以及 .groupbindforce <旧群号>
		rawGroup = cmdArgs.GetArgN(1)
		if strings.EqualFold(rawGroup, "bindforce") || strings.EqualFold(rawGroup, "force") {
			rawGroup = ""
		}
	}
	if strings.TrimSpace(rawGroup) == "" {
		ReplyToSender(ctx, msg, "请使用 `.group bindforce <旧群号>`，例如 `.group bindforce 577347791`。")
		return solved
	}
	oldGroupID, err := normalizeIdentityBindGroup(rawGroup)
	if err != nil {
		ReplyToSender(ctx, msg, err.Error())
		return solved
	}

	var issues []string
	if _, ok := identityBindStoreOf(d).find(d, oldGroupID); ok {
		issues = append(issues, fmt.Sprintf("旧群 %s 已经被绑定到另一个群了", oldGroupID))
	}
	if existing, ok := identityBindStoreOf(d).find(d, ctx.Group.GroupID); ok {
		issues = append(issues, fmt.Sprintf("本群已经绑定到 %s 了，请先 .group unbind", existing.Old.GroupID))
	}
	if len(issues) > 0 {
		ReplyToSender(ctx, msg, "无法手动确认：\n  · "+strings.Join(issues, "\n  · "))
		return solved
	}

	oldGroupName := ""
	if oldGroupObj, ok := ctx.Session.ServiceAtNew.Load(oldGroupID); ok && oldGroupObj != nil {
		oldGroupName = oldGroupObj.GroupName
	}

	record := &identityBindRecord{
		Action: identityBindActionGroup,
		New: identityBindEndpoint{
			Platform: ctx.EndPoint.Platform,
			Protocol: ctx.EndPoint.ProtocolType,
			GroupID:  ctx.Group.GroupID,
			UserID:   ctx.Player.UserID,
		},
		Old: identityBindEndpoint{
			Platform:  "QQ",
			Protocol:  "onebot",
			GroupID:   oldGroupID,
			GroupName: oldGroupName,
		},
		Created: time.Now().Unix(),
		Creator: ctx.Player.UserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("保存绑定失败: %v", err))
		return solved
	}

	// 清掉真实群上残留的日志状态（同正常绑定路径，避免回退时"复活"成空日志）
	identityBindResetRealGroupLogState(ctx, ctx.Group)

	// 顺手清掉这个群里可能挂着的、还没被确认的挑战
	identityBindCancelCode(identityBindActionGroup, ctx.Group.GroupID)

	ReplyToSender(ctx, msg, fmt.Sprintf(
		"已由骰主手动确认：本群与旧群 %s 绑定成功，双方共用同一份日志。\n"+
			"（本次跳过了验证码——请确认你确实核实过对方身份。）\n"+
			"如需解除请发送 `.group unbind`。", oldGroupID))
	ctx.Notice(fmt.Sprintf(
		"群绑定（骰主手动确认）: 群 <%s>(%s) 已绑定到旧群 %s，操作者 <%s>(%s)",
		ctx.Group.GroupName, ctx.Group.GroupID, oldGroupID, msg.Sender.Nickname, ctx.Player.UserID,
	), NoticeTypeGroup)
	d.LastUpdatedTime = time.Now().Unix()
	return solved
}

// identityBindLogWriteGroupID 计算日志「写入」应该使用的群 ID。
//
// 与读取一样归一到旧群：做了群绑定之后，官方群和旧群要把消息记进同一份日志，
// 否则两边各记各的，谁也看不到对方那半段对话。
//
// 与 identityBindLogReadGroupID 的差别：写入不检查目标群有没有历史日志
// （新日志本来就可能一条都还没有）。
func identityBindLogWriteGroupID(ctx *MsgContext, fallback string) string {
	if ctx == nil || ctx.Dice == nil {
		return fallback
	}
	current := fallback
	if current == "" && ctx.Group != nil {
		current = ctx.Group.GroupID
	}
	if current == "" {
		return fallback
	}
	target := identityCanonicalGroupID(ctx.Dice, current)
	if target == "" {
		return fallback
	}
	return target
}

// identityBindResetRealGroupLogState 清掉「真实群对象上残留的日志状态」。
//
// 背景：GroupInfo.LogCurName / LogOn 是**会持久化**的字段，而 LogCurID 不持久化
// （重启后由 ensureGroupLogState 用「本群自己的 GroupID + LogCurName」去补全）。
//
// 群绑定之后，日志状态统一挂在归一后的群（旧群）上，真实群那份状态就失去意义了。
// 如果不清掉，它会一直留在数据库里，造成两个后果：
//
//  1. 解绑（.group unbind）之后，官方群会突然"自动开始记录"一个并不存在的同名日志；
//  2. **回退到官方主线**之后，官方主线不认识绑定关系，会拿着这份残留状态执行
//     LogGetOrCreate(官方群ID, 旧群的日志名)，凭空建出一份**空日志**，
//     于是"当前故事"名字对、条数却是 0，看起来像数据丢了。
//
// 清掉它是安全的：绑定期内所有读写都走归一后的群，不依赖这份状态。
func identityBindResetRealGroupLogState(ctx *MsgContext, realGroup *GroupInfo) {
	if realGroup == nil {
		return
	}
	state := realGroup.GetLogState()
	if !state.On && state.Name == "" && state.ID == 0 {
		return
	}
	realGroup.ClearLogState()
	if ctx != nil && ctx.Dice != nil {
		realGroup.MarkDirty(ctx.Dice)
	}
}

// identityBindLogReadGroupID 计算日志「读取」应该使用的群 ID。
//
// 双向共享之后，两侧都会归一：官方群归一成旧群（历史日志在旧群），
// 旧群归一成官方群（新产生的日志在官方群）。归一到的目标群必须确实有日志记录，
// 否则保留调用方给的 fallback，避免「绑了之后旧群突然一条日志都看不到」。
//
// 只用于 list / get / stat / export 这类读取操作，写入（on / new / end）仍然使用真实当前群。
func identityBindLogReadGroupID(ctx *MsgContext, fallback string) string {
	if ctx == nil || ctx.Dice == nil {
		return fallback
	}
	current := fallback
	if current == "" && ctx.Group != nil {
		current = ctx.Group.GroupID
	}
	if current == "" {
		return fallback
	}
	target := identityCanonicalGroupID(ctx.Dice, current)
	if target == "" || target == current {
		return fallback
	}
	if !identityBindGroupHasLogs(ctx.Dice, target) {
		return fallback
	}
	return target
}

// identityBindGroupHasLogs 判断某个群里是否真的存在日志记录。
func identityBindGroupHasLogs(d *Dice, groupID string) bool {
	if d == nil || groupID == "" {
		return false
	}
	items, err := service.LogGetList(d.DBOperator, groupID)
	if err != nil {
		return false
	}
	return len(items) > 0
}

// identityBindStatusSuffix 追加到 .log 状态输出的绑定提示。
func identityBindStatusSuffix(ctx *MsgContext) string {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil {
		return ""
	}
	record, ok := identityBindStoreOf(ctx.Dice).find(ctx.Dice, ctx.Group.GroupID)
	if !ok || record.Action != identityBindActionGroup {
		return ""
	}
	return fmt.Sprintf("\n日志绑定: 读取旧群 %s", record.Old.GroupID)
}
