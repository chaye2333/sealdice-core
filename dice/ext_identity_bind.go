package dice

import (
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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
//  4. 验证题目来自旧身份的历史数据（角色卡名 / 日志名），答对后才写入绑定。

const (
	// identityBindStoreFilename 绑定关系落盘文件名，位于 data/<骰子名>/ 下。
	identityBindStoreFilename = "identity-bindings.json"

	// identityBindSessionTTL 一次问答会话的有效期。
	identityBindSessionTTL = 10 * time.Minute

	// identityBindQuestionOptionCount 每道题给出的选项个数。
	identityBindQuestionOptionCount = 4

	// identityBindMaxQuestionCount 允许配置的最大题目数量。
	identityBindMaxQuestionCount = 5

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

// identityBindQuestionKind 题目类型。
type identityBindQuestionKind int

const (
	// identityBindQuestionChoice 选择题：给出若干选项，回复序号。
	identityBindQuestionChoice identityBindQuestionKind = iota
	// identityBindQuestionText 填空题：直接输入名称，避免用假的干扰项把答案暴露出来。
	identityBindQuestionText
)

// identityBindQuestion 一道验证题。选项在生成时已打乱。
type identityBindQuestion struct {
	Kind identityBindQuestionKind
	// Kind == choice 时有效
	Prompt  string
	Options []string
	Answer  int
	// 所有可接受的答案（用于填空，以及「本人任意一张卡名都算对」的情况）
	Answers []string
}

// identityBindSession 一次等待作答的绑定会话。
type identityBindSession struct {
	Action    identityBindAction
	New       identityBindEndpoint
	Old       identityBindEndpoint
	Questions []identityBindQuestion
	Created   int64
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

// identityBindQuestionCount 取配置的题目数量并收敛到合法范围。
func identityBindQuestionCount(d *Dice) int {
	if d == nil {
		return 1
	}
	count := int(d.Config.IdentityBindQuestionCount)
	if count <= 0 {
		count = int(DefaultConfig.IdentityBindQuestionCount)
	}
	if count > identityBindMaxQuestionCount {
		count = identityBindMaxQuestionCount
	}
	if count < 1 {
		count = 1
	}
	return count
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

// identityBindFailCooldown 取「答错后的锁定时长」并收敛到合法范围。
func identityBindFailCooldown(d *Dice) time.Duration {
	if d == nil {
		return 0
	}
	sec := d.Config.IdentityBindFailCooldownSec
	if sec <= 0 {
		return 0
	}
	if sec > identityBindMaxCooldownSec {
		sec = identityBindMaxCooldownSec
	}
	return time.Duration(sec) * time.Second
}

// identityBindFailKey 答错锁定使用单独的键，避免和常规冷却互相覆盖。
func identityBindFailKey(epID, userID string, action identityBindAction) string {
	return identityBindSessionKey(epID, userID, action) + "|failed"
}

// identityBindFailCooldownRemaining 返回答错锁定还剩多久；未被锁定返回 0。
func identityBindFailCooldownRemaining(d *Dice, epID, userID string, action identityBindAction) time.Duration {
	cooldown := identityBindFailCooldown(d)
	if cooldown <= 0 {
		return 0
	}
	value, ok := globalIdentityBindLastAttempt.Load(identityBindFailKey(epID, userID, action))
	if !ok || value == nil {
		return 0
	}
	elapsed := time.Since(time.Unix(value.At, 0))
	if elapsed >= cooldown {
		return 0
	}
	return cooldown - elapsed
}

// identityBindMarkFailed 记录一次答错，进入锁定。
func identityBindMarkFailed(epID, userID string, action identityBindAction) {
	globalIdentityBindLastAttempt.Store(
		identityBindFailKey(epID, userID, action),
		&identityBindLastAttempt{At: time.Now().Unix()},
	)
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
		key := identityBindRecordKeyFromRecord(record)
		if key == "" {
			continue
		}
		s.byKey[key] = record
	}
}

// saveLocked 落盘，调用方必须已持有写锁。
func (s *identityBindStore) saveLocked(d *Dice) error {
	records := make([]*identityBindRecord, 0, len(s.byKey))
	for _, record := range s.byKey {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return identityBindRecordKeyFromRecord(records[i]) < identityBindRecordKeyFromRecord(records[j])
	})
	payload := identityBindStoreData{
		Version: 1,
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

func identityBindRecordKey(action identityBindAction, receiverID string) string {
	return string(action) + "|" + receiverID
}

func identityBindRecordKeyFromRecord(record *identityBindRecord) string {
	if record == nil {
		return ""
	}
	if record.Key != "" {
		return record.Key
	}
	// 兜底：兼容手写或早期版本产生的文件。
	if record.Action == identityBindActionUser {
		if record.New.UserID == "" {
			return ""
		}
		return identityBindUserKey(record.New.UserID)
	}
	if record.New.GroupID == "" {
		return ""
	}
	return identityBindGroupKey(record.New.GroupID)
}

// identityBindGroupKey 群日志绑定的键：绑定挂在「接收方所在的当前群」上。
func identityBindGroupKey(groupID string) string {
	return identityBindRecordKey(identityBindActionGroup, groupID)
}

// identityBindUserKey 用户身份绑定的键：官方 QQ 的 MemberOpenID 本身就按群独立。
func identityBindUserKey(userID string) string {
	return identityBindRecordKey(identityBindActionUser, userID)
}

// put 写入/覆盖一条绑定并落盘。
func (s *identityBindStore) put(d *Dice, record *identityBindRecord) error {
	if record == nil {
		return errors.New("绑定内容为空")
	}
	key := identityBindRecordKeyFromRecord(record)
	if key == "" {
		return errors.New("绑定内容缺失关键 ID")
	}
	record.Key = key
	s.ensureLoaded(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byKey == nil {
		s.byKey = map[string]*identityBindRecord{}
	}
	s.byKey[key] = record
	return s.saveLocked(d)
}

// delete 删除一条绑定并落盘，返回是否真的删掉了。
func (s *identityBindStore) delete(d *Dice, key string) (bool, error) {
	s.ensureLoaded(d)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[key]; !ok {
		return false, nil
	}
	delete(s.byKey, key)
	return true, s.saveLocked(d)
}

func (s *identityBindStore) get(d *Dice, key string) (*identityBindRecord, bool) {
	s.ensureLoaded(d)
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.byKey[key]
	if !ok || record == nil {
		return nil, false
	}
	copied := *record
	return &copied, true
}

// list 返回所有绑定，用于 .bind list 展示。
func (s *identityBindStore) list(d *Dice) []*identityBindRecord {
	s.ensureLoaded(d)
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]*identityBindRecord, 0, len(s.byKey))
	for _, record := range s.byKey {
		if record == nil {
			continue
		}
		copied := *record
		records = append(records, &copied)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Created < records[j].Created })
	return records
}

// ---------- 解析（回退读取） ----------

// identityBindResolveGroup 解析当前群应该读取哪一份数据。
func identityBindResolveGroup(ctx *MsgContext) (*GroupInfo, bool) {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || ctx.Session == nil {
		return nil, false
	}
	if !identityBindSupported(ctx.EndPoint) {
		return nil, false
	}
	record, ok := identityBindStoreOf(ctx.Dice).get(ctx.Dice, identityBindGroupKey(ctx.Group.GroupID))
	if !ok || record.Action != identityBindActionGroup {
		return nil, false
	}
	oldGroupID := record.Old.GroupID
	if oldGroupID == "" || oldGroupID == ctx.Group.GroupID {
		return nil, false
	}
	oldGroup, exists := ctx.Session.ServiceAtNew.Load(oldGroupID)
	if !exists || oldGroup == nil {
		// 旧群没有内存记录时不能凭空造一个，回退到当前群，避免读不到数据还报错。
		return nil, false
	}
	return oldGroup, true
}

// identityBindResolveUserID 解析当前用户应该使用哪个旧 ID 去读取数据。
func identityBindResolveUserID(ctx *MsgContext) (string, bool) {
	if ctx == nil || ctx.Dice == nil || ctx.Player == nil {
		return "", false
	}
	if !identityBindSupported(ctx.EndPoint) {
		return "", false
	}
	record, ok := identityBindStoreOf(ctx.Dice).get(ctx.Dice, identityBindUserKey(ctx.Player.UserID))
	if !ok || record.Action != identityBindActionUser {
		return "", false
	}
	oldUserID := record.Old.UserID
	if oldUserID == "" || oldUserID == ctx.Player.UserID {
		return "", false
	}
	return oldUserID, true
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
// 权限、昵称展示等仍然使用 ctx.Player（真实新身份）。
func identityBindReadPlayer(ctx *MsgContext) *GroupPlayerInfo {
	if ctx == nil || ctx.Dice == nil {
		return nil
	}
	oldUserID, bound := identityBindResolveUserID(ctx)
	if !bound {
		return ctx.Player
	}
	readGroup, _ := identityBindReadGroup(ctx)
	if readGroup == nil {
		return ctx.Player
	}
	player := readGroup.PlayerGet(ctx.Dice.DBOperator, oldUserID)
	if player == nil {
		// 旧身份在本群还没有记录时，不能伪造数据，回退到当前玩家。
		return ctx.Player
	}
	return player
}

// identityBindAttrTarget 计算属性读写应该使用的 群ID / 用户ID。
// 返回 bound=false 时表示没有绑定，调用方按原逻辑处理。
func identityBindAttrTarget(ctx *MsgContext) (groupID string, userID string, bound bool) {
	if ctx == nil || ctx.Group == nil || ctx.Player == nil {
		return "", "", false
	}
	oldUserID, userBound := identityBindResolveUserID(ctx)
	readGroup, groupBound := identityBindResolveGroup(ctx)
	if !userBound && !groupBound {
		return "", "", false
	}
	groupID = ctx.Group.GroupID
	if groupBound && readGroup != nil {
		groupID = readGroup.GroupID
	}
	userID = ctx.Player.UserID
	if userBound {
		userID = oldUserID
	}
	if groupID == ctx.Group.GroupID && userID == ctx.Player.UserID {
		return "", "", false
	}
	return groupID, userID, true
}

// ---------- 会话与冷却 ----------

func identityBindSessionKey(epID, userID string, action identityBindAction) string {
	return epID + "|" + userID + "|" + string(action)
}

func identityBindCleanupSessions() {
	now := time.Now().Unix()
	globalIdentityBindSessions.Range(func(key string, value *identityBindSession) bool {
		if value == nil || now-value.Created > int64(identityBindSessionTTL/time.Second) {
			globalIdentityBindSessions.Delete(key)
		}
		return true
	})
}

func identityBindStoreSession(key string, session *identityBindSession) {
	identityBindCleanupSessions()
	globalIdentityBindSessions.Store(key, session)
}

func identityBindLoadSession(key string) (*identityBindSession, bool) {
	session, ok := globalIdentityBindSessions.Load(key)
	if !ok || session == nil {
		return nil, false
	}
	if time.Now().Unix()-session.Created > int64(identityBindSessionTTL/time.Second) {
		globalIdentityBindSessions.Delete(key)
		return nil, false
	}
	return session, true
}

func identityBindClearSession(key string) {
	globalIdentityBindSessions.Delete(key)
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

	// 答错锁定优先返回（通常远长于常规冷却）
	return identityBindFailCooldownRemaining(d, epID, userID, action)
}

func identityBindMarkAttempt(epID, userID string, action identityBindAction) {
	globalIdentityBindLastAttempt.Store(
		identityBindSessionKey(epID, userID, action),
		&identityBindLastAttempt{At: time.Now().Unix()},
	)
}

// ---------- 出题 ----------

// identityBindBuildQuestions 从候选名里出题。
// candidates 是「属于该身份的正确答案」，decoys 是用来干扰的错误选项。
func identityBindBuildQuestions(
	prefix string,
	candidates []string,
	decoys []string,
	count int,
) ([]identityBindQuestion, bool) {
	names := dedupeNonEmpty(candidates)
	if len(names) == 0 {
		return nil, false
	}
	pool := dedupeNonEmpty(decoys)
	// 正确答案本身也可以当作其它题目的干扰项
	pool = dedupeNonEmpty(append(pool, names...))

	if count < 1 {
		count = 1
	}
	if count > len(names) {
		count = len(names)
	}

	shuffledNames := shuffleStrings(names)
	selected := shuffledNames[:count]

	questions := make([]identityBindQuestion, 0, count)
	for _, answer := range selected {
		distractors := make([]string, 0, len(pool))
		for _, name := range pool {
			if name == answer {
				continue
			}
			distractors = append(distractors, name)
		}
		distractors = shuffleStrings(distractors)
		want := identityBindQuestionOptionCount - 1
		if len(distractors) < want {
			want = len(distractors)
		}
		options := append([]string{answer}, distractors[:want]...)
		options = shuffleStrings(options)
		answerIndex := 0
		for i, option := range options {
			if option == answer {
				answerIndex = i
				break
			}
		}
		questions = append(questions, identityBindQuestion{
			Kind:    identityBindQuestionChoice,
			Prompt:  fmt.Sprintf("%s（选项：%s）", prefix, strings.Join(options, " / ")),
			Options: options,
			Answer:  answerIndex,
			// 所有候选名都算对：比如玩家有多张卡，问其中一张时选另一张也算他知道自己的卡
			Answers: names,
		})
	}
	return questions, len(questions) > 0
}

// identityBindBuildTextQuestion 生成「填空」题：直接把名字列出来让玩家回答。
//
// 日志名这类信息如果用选择项，干扰项要么是假的（一眼看穿）、要么来自别人的群
// （泄露信息），所以改为填空——玩家必须真的知道旧群里有哪些日志名。
func identityBindBuildTextQuestion(prefix string, candidates []string) (identityBindQuestion, bool) {
	names := shuffleStrings(dedupeNonEmpty(candidates))
	if len(names) == 0 {
		return identityBindQuestion{}, false
	}
	return identityBindQuestion{
		Kind:    identityBindQuestionText,
		Prompt:  fmt.Sprintf("%s（本群共有 %d 个日志：%s；回复其中一个即可）", prefix, len(names), strings.Join(names, " / ")),
		Answers: names,
	}, true
}

func dedupeNonEmpty(items []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}

func shuffleStrings(items []string) []string {
	result := make([]string, len(items))
	copy(result, items)
	for i := len(result) - 1; i > 0; i-- {
		var j int
		if n, err := crand.Int(crand.Reader, big.NewInt(int64(i+1))); err == nil {
			j = int(n.Int64())
		} else {
			j = int(time.Now().UnixNano()) % (i + 1)
		}
		result[i], result[j] = result[j], result[i]
	}
	return result
}

// identityBindCardNames 查询某个旧用户在其所有群里的角色卡名。
func identityBindCardNames(d *Dice, oldUserID string) ([]string, error) {
	if d == nil || d.AttrsManager == nil {
		return nil, errors.New("属性管理器未初始化")
	}
	cards, err := d.AttrsManager.GetCharacterList(oldUserID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cards))
	for _, card := range cards {
		if card == nil || card.IsHidden {
			continue
		}
		names = append(names, card.Name)
	}
	return dedupeNonEmpty(names), nil
}

// identityBindDecoyCardNames 取一些「别人」的卡名作为干扰项。
// 只从内存缓存里取，避免为了出题全表扫描数据库；取不到时不影响出题。
func identityBindDecoyCardNames(d *Dice, limit int) []string {
	if d == nil || d.AttrsManager == nil || limit <= 0 {
		return nil
	}
	names := make([]string, 0, limit)
	d.AttrsManager.m.Range(func(_ string, item *AttributesItem) bool {
		if item == nil || item.Name == "" {
			return true
		}
		names = append(names, item.Name)
		return len(names) < limit
	})
	return dedupeNonEmpty(names)
}

// identityBindLogNames 查询某个旧群已有的日志名。
func identityBindLogNames(d *Dice, oldGroupID string) ([]string, error) {
	if d == nil || d.DBOperator == nil {
		return nil, errors.New("数据库未初始化")
	}
	names, err := service.LogGetList(d.DBOperator, oldGroupID)
	if err != nil {
		return nil, err
	}
	return dedupeNonEmpty(names), nil
}

// ---------- 会话输出 ----------

func identityBindFormatQuestions(questions []identityBindQuestion) string {
	hasText := false
	for _, question := range questions {
		if question.Kind == identityBindQuestionText {
			hasText = true
			break
		}
	}

	lines := make([]string, 0, len(questions)+2)
	if hasText {
		lines = append(lines, "请按顺序回答下面的问题，直接回复答案文字即可：")
	} else {
		lines = append(lines, "请按顺序回答下面的问题，把每题的选项序号连起来回复即可，例如 `1234`：")
	}
	for i, question := range questions {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, question.Prompt))
	}
	lines = append(lines, "回复 `.bind cancel` 可以取消本次绑定；答错会被锁定，请勿尝试猜测。")
	return strings.Join(lines, "\n")
}

// identityBindParseAnswers 解析用户回复的选项序号。
// 支持 "132"、"1 3 2"、"1,3,2" 三种写法。
func identityBindParseAnswers(text string, questionCount int) ([]int, bool) {
	digits := make([]int, 0, questionCount)
	for _, r := range text {
		switch {
		case r >= '1' && r <= '9':
			digits = append(digits, int(r-'0'))
		case r == '0' || r == ' ' || r == ',' || r == '，' || r == '-' || r == '/':
			// 分隔符与 0 直接忽略：选项序号从 1 开始
		default:
			return nil, false
		}
	}
	if len(digits) != questionCount {
		return nil, false
	}
	return digits, true
}

// identityBindCheckAnswers 校验选择题答案，全部正确才算通过。
func identityBindCheckAnswers(questions []identityBindQuestion, answers []int) bool {
	if len(questions) != len(answers) {
		return false
	}
	for i, question := range questions {
		if question.Kind != identityBindQuestionChoice {
			return false
		}
		// 选项序号从 1 开始
		if answers[i]-1 != question.Answer {
			return false
		}
	}
	return true
}

// identityBindCheckTextAnswers 校验填空题答案（大小写不敏感，忽略首尾空白）。
// 任意一个可接受答案命中即可。
func identityBindCheckTextAnswers(questions []identityBindQuestion, answers []string) bool {
	if len(questions) != len(answers) {
		return false
	}
	for i, question := range questions {
		if question.Kind != identityBindQuestionText {
			return false
		}
		given := strings.TrimSpace(answers[i])
		if given == "" {
			return false
		}
		hit := false
		for _, want := range question.Answers {
			if strings.EqualFold(strings.TrimSpace(want), given) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// identityBindSplitTextAnswer 把用户回复拆成「每题的填空答案」。
// 约定：单题用整段文本；多题用 " / " 或空格分隔。
func identityBindSplitTextAnswer(raw string, questionCount int) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if questionCount <= 1 {
		return []string{raw}
	}
	for _, sep := range []string{" / ", "/", "，", ","} {
		if strings.Contains(raw, sep) {
			parts := strings.Split(raw, sep)
			result := make([]string, 0, len(parts))
			for _, part := range parts {
				if trimmed := strings.TrimSpace(part); trimmed != "" {
					result = append(result, trimmed)
				}
			}
			if len(result) == questionCount {
				return result
			}
		}
	}
	if parts := strings.Fields(raw); len(parts) == questionCount {
		return parts
	}
	return nil
}

// ---------- .bind 指令 ----------

func identityBindUserHelp() string {
	return `.bind <旧QQ号> // 绑定你的个人身份，之后你的角色卡/属性都从旧 QQ 号读取
.bind <选项序号> // 在问答过程中提交答案，例如 .bind 132
.bind cancel // 取消进行中的问答
.bind reset // 同上，顺便清掉群绑定的问答
.bind status // 查看自己的绑定
.bind list // 查看所有绑定记录，需要管理权限
.unbind // 解除自己的个人绑定

个人绑定与群无关、全局生效：在任何官方群里绑定一次即可。
旧群号只是可选参数（.bind <旧QQ号> <旧群号>），填了也只会用于展示，不影响验证。

群绑定（把整个群指向旧群，日志读取随之前移，需要管理权限）：
.group bind <旧群号>
.group unbind
.group status
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

	// 已有问答会话时，参数当作答案
	if session, ok := identityBindLoadSession(sessionKey); ok {
		// 兼容 .bind bind 1 / .bindb 这类写法，把 bind 关键字去掉再解析
		raw := strings.Join(cmdArgs.Args, "")
		if trimmed, cut := strings.CutPrefix(strings.ToLower(raw), "bind"); cut {
			raw = trimmed
		}
		if strings.EqualFold(strings.TrimSpace(raw), "cancel") {
			return identityBindCancelSession(ctx, msg, false)
		}
		if strings.TrimSpace(raw) == "" {
			ReplyToSender(ctx, msg, identityBindFormatQuestions(session.Questions))
			return solved
		}
		parsed, valid := identityBindParseAnswers(raw, len(session.Questions))
		if !valid {
			ReplyToSender(ctx, msg, fmt.Sprintf("答案格式不正确，请给出 %d 个选项序号，例如 `%s`。",
				len(session.Questions), strings.Repeat("1", len(session.Questions))))
			return solved
		}
		return identityBindVerifyUserAnswers(ctx, msg, sessionKey, session, parsed)
	}

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

// identityBindCancelSession 取消（清空）当前用户的绑定问答会话。
// clearAll 为 true 时同时清掉个人与群两种会话。
func identityBindCancelSession(ctx *MsgContext, msg *Message, clearAll bool) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.EndPoint == nil || ctx.Player == nil {
		return solved
	}

	actions := []identityBindAction{identityBindActionUser}
	if clearAll {
		actions = append(actions, identityBindActionGroup)
	}

	cleared := 0
	for _, action := range actions {
		key := identityBindSessionKey(ctx.EndPoint.ID, ctx.Player.UserID, action)
		if _, ok := identityBindLoadSession(key); ok {
			cleared++
		}
		identityBindClearSession(key)
	}

	if cleared == 0 {
		ReplyToSender(ctx, msg, "当前没有进行中的绑定问答。")
		return solved
	}
	ReplyToSender(ctx, msg, "已取消进行中的绑定问答。绑定记录本身没有被修改，可以随时重新发起。")
	return solved
}

// identityBindStartUserSession 发起个人身份绑定。
//
// 注意：个人身份绑定是**全局跨群通用**的——官方 QQ 的 MemberOpenID 本身就按群独立，
// 但绑定的目标是"这个人的旧 QQ 号"，与旧群无关。角色卡是按 owner_id（旧 QQ 号）
// 查询的，所以不需要旧群号。旧群号只是可选参数，用来展示更友好的提示。
func identityBindStartUserSession(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs, sessionKey, oldUserID string) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if remaining := identityBindCooldownRemaining(d, ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionUser); remaining > 0 {
		ReplyToSender(ctx, msg, fmt.Sprintf("操作过于频繁，请在 %d 秒后重试。", int(remaining.Seconds())+1))
		return solved
	}

	if existing, ok := identityBindStoreOf(d).get(d, identityBindUserKey(ctx.Player.UserID)); ok {
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

	questions, errText := identityBindBuildUserQuestions(d, oldUserID)
	if errText != "" {
		identityBindMarkAttempt(ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionUser)
		ReplyToSender(ctx, msg, errText)
		return solved
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

	session := &identityBindSession{
		Action: identityBindActionUser,
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
			UserID:    oldUserID,
			GroupName: oldGroupName,
			UserName:  oldPlayerName,
		},
		Questions: questions,
		Created:   time.Now().Unix(),
	}
	identityBindStoreSession(sessionKey, session)
	identityBindMarkAttempt(ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionUser)

	ReplyToSender(ctx, msg, fmt.Sprintf("正在验证旧身份 %s，共 %d 题。\n%s",
		oldUserID, len(questions), identityBindFormatQuestions(questions)))
	return solved
}

// identityBindBuildUserQuestions 依据旧用户的角色卡名出题。
func identityBindBuildUserQuestions(d *Dice, oldUserID string) ([]identityBindQuestion, string) {
	names, err := identityBindCardNames(d, oldUserID)
	if err != nil {
		return nil, fmt.Sprintf("查询旧数据失败: %v", err)
	}
	if len(names) == 0 {
		return nil, fmt.Sprintf(
			"找不到 %s 的角色卡，无法出题验证。请确认旧 QQ 号填写正确，或联系骰主人工处理。",
			oldUserID,
		)
	}
	decoys := identityBindDecoyCardNames(d, 16)
	questions, ok := identityBindBuildQuestions("你的角色卡名是", names, decoys, identityBindQuestionCount(d))
	if !ok {
		return nil, "生成验证题目失败，请联系骰主处理。"
	}
	return questions, ""
}

func identityBindVerifyUserAnswers(ctx *MsgContext, msg *Message, sessionKey string, session *identityBindSession, answers []int) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if !identityBindCheckAnswers(session.Questions, answers) {
		identityBindClearSession(sessionKey)
		identityBindMarkFailed(ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionUser)
		lock := identityBindFailCooldown(d)
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"答案不正确，本次绑定未通过。为防止猜测，%s内不能再发起绑定。",
			identityBindFormatDuration(lock)))
		ctx.Notice(fmt.Sprintf(
			"身份绑定验证失败: 群 <%s>(%s) 用户 <%s>(%s) 尝试绑定到 %s（已锁定 %s）",
			ctx.Group.GroupName, ctx.Group.GroupID, msg.Sender.Nickname, ctx.Player.UserID,
			session.Old.UserID, identityBindFormatDuration(lock),
		), NoticeTypeGroup)
		return solved
	}

	record := &identityBindRecord{
		Action:  identityBindActionUser,
		Key:     identityBindUserKey(session.New.UserID),
		New:     session.New,
		Old:     session.Old,
		Created: time.Now().Unix(),
		Creator: ctx.Player.UserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("保存绑定失败: %v", err))
		return solved
	}
	identityBindClearSession(sessionKey)

	ReplyToSender(ctx, msg, fmt.Sprintf(
		"绑定成功！\n当前身份 %s 现在会读取 %s 的历史数据。\n如需解除请发送 `.unbind`。",
		ctx.Player.UserID, session.Old.UserID,
	))
	ctx.Notice(fmt.Sprintf(
		"身份绑定成功: 群 <%s>(%s) 用户 <%s>(%s) 已绑定到旧身份 %s（旧群 %s）",
		ctx.Group.GroupName, ctx.Group.GroupID, msg.Sender.Nickname, ctx.Player.UserID,
		session.Old.UserID, session.Old.GroupID,
	), NoticeTypeGroup)
	d.LastUpdatedTime = time.Now().Unix()
	return solved
}

// identityBindFormatUserStatus 生成个人绑定状态。
// 只展示「个人身份绑定」的信息；群绑定请用 .group status 查看，两者是独立的。
func identityBindFormatUserStatus(d *Dice, ctx *MsgContext) string {
	lines := []string{
		"【个人身份绑定】把你自己指向迁移前的旧 QQ 号，之后你的角色卡/属性都从旧身份读取。",
		"作用范围: 全局（与群无关，绑定一次所有官方群通用）",
		fmt.Sprintf("当前身份: %s", ctx.Player.UserID),
	}
	if record, ok := identityBindStoreOf(d).get(d, identityBindUserKey(ctx.Player.UserID)); ok {
		lines = append(lines, fmt.Sprintf("已绑定旧QQ号: %s", record.Old.UserID))
		if record.Old.UserName != "" {
			lines = append(lines, fmt.Sprintf("旧群内昵称: %s", record.Old.UserName))
		}
		if record.Old.GroupID != "" {
			lines = append(lines, fmt.Sprintf("绑定时填写的旧群: %s", record.Old.GroupID))
		}
		lines = append(lines, fmt.Sprintf("绑定时间: %s", time.Unix(record.Created, 0).Format("2006-01-02 15:04")))
		lines = append(lines, "解除方式: .unbind")
	} else {
		lines = append(lines, "尚未绑定旧QQ号")
		lines = append(lines, "发起绑定: .bind <旧QQ号>")
	}
	// 顺带提示本群的群绑定状态，避免两个功能混淆
	if ctx.Group != nil {
		if record, ok := identityBindStoreOf(d).get(d, identityBindGroupKey(ctx.Group.GroupID)); ok {
			lines = append(lines, fmt.Sprintf("（本群群绑定: 旧群 %s，用 .group status 查看）", record.Old.GroupID))
		} else {
			lines = append(lines, "（本群未做群绑定，群绑定用 .group bind <旧群号>）")
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
			kind, record.New.UserID, record.Old.UserID,
			time.Unix(record.Created, 0).Format("2006-01-02 15:04"),
		))
	}
	return strings.Join(lines, "\n")
}

// identityBindRunUnbind 处理 .unbind。
func identityBindRunUnbind(ctx *MsgContext, msg *Message, action identityBindAction) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if !identityBindSupported(ctx.EndPoint) {
		ReplyToSender(ctx, msg, "身份绑定仅用于 QQ 官方机器人，当前连接方式无需解绑。")
		return solved
	}

	key := identityBindUserKey(ctx.Player.UserID)
	if action == identityBindActionGroup {
		key = identityBindGroupKey(ctx.Group.GroupID)
	}
	deleted, err := identityBindStoreOf(d).delete(d, key)
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
	return `【群绑定】把当前官方群指向迁移前的旧群，之后本群的日志读取
（.log list / .log get / .log stat / .log export）都从旧群取。

.group bind <旧群号> // 发起群绑定（需要管理权限）
.group bind <选项序号> // 在问答过程中提交答案
.group cancel // 取消进行中的问答（同时清掉个人绑定的问答）
.group unbind // 解除当前群的绑定（需要管理权限）
.group status // 查看当前群的绑定

说明：
* 群绑定只影响「日志读取」；写入（.log new / on / end）仍然记在当前群。
* 群绑定与个人身份绑定（.bind）完全独立，可以同时使用，互不影响。
* 兼容写法：.log bind / .log unbind / .log bindstatus 与上面等价。`
}

// identityBindGroupStatus 生成当前群的绑定状态。
// 只展示「群绑定」的信息；个人身份绑定请用 .bind status 查看，两者是独立的。
func identityBindGroupStatus(d *Dice, ctx *MsgContext) string {
	lines := []string{
		"【群绑定】把整个群指向迁移前的旧群，之后本群的日志读取（.log list / get / stat / export）都从旧群取。",
		fmt.Sprintf("当前群: %s", ctx.Group.GroupID),
	}
	if record, ok := identityBindStoreOf(d).get(d, identityBindGroupKey(ctx.Group.GroupID)); ok {
		lines = append(lines, fmt.Sprintf("已绑定旧群: %s", record.Old.GroupID))
		if record.Old.GroupName != "" {
			lines = append(lines, fmt.Sprintf("旧群名称: %s", record.Old.GroupName))
		}
		lines = append(lines, fmt.Sprintf("绑定时间: %s", time.Unix(record.Created, 0).Format("2006-01-02 15:04")))
		lines = append(lines, "解除方式: .group unbind")
	} else {
		lines = append(lines, "尚未绑定旧群")
		lines = append(lines, "发起绑定: .group bind <旧群号>")
	}
	lines = append(lines, "（个人身份绑定与群绑定互相独立，用 .bind status 查看个人绑定）")
	return strings.Join(lines, "\n")
}

// runIdentityBindGroupCommand 处理全局指令 .group。
//
// 支持：
//
//	.group bind <旧群号>   // 发起群绑定（需要管理权限）
//	.group bind <选项序号> // 在问答过程中提交答案
//	.group unbind          // 解除当前群的绑定（需要管理权限）
//	.group status          // 查看当前群的绑定
//
// 群绑定与用户身份绑定（.bind）完全独立，互不影响。
func runIdentityBindGroupCommand(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
	solved := CmdExecuteResult{Matched: true, Solved: true}
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil {
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

	// 取消 / 重置：群绑定和个人身份绑定的问答都会被清掉
	if sub == "cancel" || sub == "reset" {
		return identityBindCancelSession(ctx, msg, true)
	}

	// .group status 查看当前群绑定
	if sub == "status" || sub == "bindstatus" {
		ReplyToSender(ctx, msg, identityBindGroupStatus(ctx.Dice, ctx))
		return solved
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
	case "bind", "unbind", "bindstatus":
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

	sessionKey := identityBindSessionKey(ctx.EndPoint.ID, ctx.Player.UserID, action)

	// 统一取第一个参数作为子指令：.group bind / .group / .log bind 都适用
	sub := strings.ToLower(cmdArgs.GetArgN(1))
	if sub != "bind" {
		sub = ""
	}

	// 已有会话时，参数是答案
	// 消息形如 `.group bind 123` / `.log bind 123` / `.group 123`
	if session, ok := identityBindLoadSession(sessionKey); ok {
		raw := strings.Join(cmdArgs.Args, "")
		// 把开头的 bind 或 group 关键字去掉再解析
		for _, keyword := range []string{"bind", "group", "groupbind", "log"} {
			if trimmed, cut := strings.CutPrefix(strings.ToLower(raw), keyword); cut {
				raw = trimmed
				break
			}
		}
		raw = strings.TrimSpace(raw)
		if strings.EqualFold(raw, "cancel") || strings.EqualFold(raw, "reset") {
			return identityBindCancelSession(ctx, msg, false)
		}
		if raw == "" {
			ReplyToSender(ctx, msg, identityBindFormatQuestions(session.Questions))
			return solved
		}
		// 日志题是填空题：直接比对日志名
		textAnswers := identityBindSplitTextAnswer(raw, len(session.Questions))
		if textAnswers == nil {
			ReplyToSender(ctx, msg, "请直接回复日志名（多题用 ` / ` 分隔）。")
			return solved
		}
		return identityBindVerifyLogAnswers(ctx, msg, sessionKey, session, textAnswers)
	}

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

	if existing, ok := identityBindStoreOf(d).get(d, identityBindGroupKey(ctx.Group.GroupID)); ok {
		ReplyToSender(ctx, msg, fmt.Sprintf("本群已经绑定到 %s 了，如需更换请先发送 `.group unbind`。", existing.Old.GroupID))
		return solved
	}

	names, err := identityBindLogNames(d, oldGroupID)
	if err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("查询旧群日志失败: %v", err))
		return solved
	}
	if len(names) == 0 {
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"旧群 %s 没有任何日志记录，无法出题验证。请确认旧群号正确，或该群此前确实记录过日志。", oldGroupID))
		return solved
	}

	// 日志题用填空：不用假的干扰项，避免「[其它群]」这种一眼可辨的选项让题目失去意义
	question, ok := identityBindBuildTextQuestion("这是你们团在旧群的日志名，请回复其中一个", names)
	if !ok {
		ReplyToSender(ctx, msg, "生成验证题目失败，请联系骰主处理。")
		return solved
	}
	questions := []identityBindQuestion{question}

	oldGroupName := ""
	if group, exists := ctx.Session.ServiceAtNew.Load(oldGroupID); exists && group != nil {
		oldGroupName = group.GroupName
	}

	session := &identityBindSession{
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
		Questions: questions,
		Created:   time.Now().Unix(),
	}
	identityBindStoreSession(sessionKey, session)
	identityBindMarkAttempt(ctx.EndPoint.ID, ctx.Player.UserID, action)

	ReplyToSender(ctx, msg, fmt.Sprintf("正在验证旧群 %s，共 %d 题。\n%s",
		oldGroupID, len(questions), identityBindFormatQuestions(questions)))
	return solved
}

func identityBindVerifyLogAnswers(ctx *MsgContext, msg *Message, sessionKey string, session *identityBindSession, answers []string) CmdExecuteResult {
	d := ctx.Dice
	solved := CmdExecuteResult{Matched: true, Solved: true}

	if !identityBindCheckTextAnswers(session.Questions, answers) {
		identityBindClearSession(sessionKey)
		identityBindMarkFailed(ctx.EndPoint.ID, ctx.Player.UserID, identityBindActionGroup)
		lock := identityBindFailCooldown(d)
		ReplyToSender(ctx, msg, fmt.Sprintf(
			"答案不正确，本次群绑定未通过。为防止猜测，%s内不能再发起绑定。",
			identityBindFormatDuration(lock)))
		ctx.Notice(fmt.Sprintf(
			"群绑定验证失败: 群 <%s>(%s) 用户 <%s>(%s) 尝试绑定到旧群 %s（已锁定 %s）",
			ctx.Group.GroupName, ctx.Group.GroupID, msg.Sender.Nickname, ctx.Player.UserID,
			session.Old.GroupID, identityBindFormatDuration(lock),
		), NoticeTypeGroup)
		return solved
	}

	record := &identityBindRecord{
		Action:  identityBindActionGroup,
		Key:     identityBindGroupKey(session.New.GroupID),
		New:     session.New,
		Old:     session.Old,
		Created: time.Now().Unix(),
		Creator: ctx.Player.UserID,
	}
	if err := identityBindStoreOf(d).put(d, record); err != nil {
		ReplyToSender(ctx, msg, fmt.Sprintf("保存日志绑定失败: %v", err))
		return solved
	}
	identityBindClearSession(sessionKey)

	ReplyToSender(ctx, msg, fmt.Sprintf(
		"日志绑定成功！当前群现在会读取旧群 %s 的日志记录。\n如需解除请发送 `.group unbind`。",
		session.Old.GroupID,
	))
	ctx.Notice(fmt.Sprintf(
		"日志绑定成功: 群 <%s>(%s) 已绑定到旧群 %s，操作者 <%s>(%s)",
		ctx.Group.GroupName, ctx.Group.GroupID, session.Old.GroupID, msg.Sender.Nickname, ctx.Player.UserID,
	), NoticeTypeGroup)
	d.LastUpdatedTime = time.Now().Unix()
	return solved
}

// identityBindLogReadGroupID 计算日志「读取」应该使用的群 ID。
// 当前群绑定了旧群时返回旧群 ID，否则原样返回。
// 只用于 list / get / stat / export 这类读取操作，写入（on / new / end）仍然使用当前群。
func identityBindLogReadGroupID(ctx *MsgContext, fallback string) string {
	if ctx == nil || ctx.Dice == nil {
		return fallback
	}
	group, bound := identityBindResolveGroup(ctx)
	if !bound || group == nil || group.GroupID == "" {
		return fallback
	}
	return group.GroupID
}

// identityBindStatusSuffix 追加到 .log 状态输出的绑定提示。
func identityBindStatusSuffix(ctx *MsgContext) string {
	if ctx == nil || ctx.Dice == nil || ctx.Group == nil || !identityBindSupported(ctx.EndPoint) {
		return ""
	}
	record, ok := identityBindStoreOf(ctx.Dice).get(ctx.Dice, identityBindGroupKey(ctx.Group.GroupID))
	if !ok {
		return ""
	}
	return fmt.Sprintf("\n日志绑定: 读取旧群 %s", record.Old.GroupID)
}
