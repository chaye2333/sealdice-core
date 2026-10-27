package dice

// 官方 QQ 聊天控制台：骰主在 WebUI 里以机器人身份查看 / 发送消息。
//
// 设计要点（详见 docs/official-qq-chat-console-design.md）：
//   - 只覆盖**官方 QQ**（OpenQQ）这一侧，不碰民间 bot（OneBot）；
//   - 不落数据库：所有数据在 <dataDir>/chat-console/ 下按会话 + 按天存 jsonl，
//     删掉整个目录 = 清空历史，旧版本程序不认识该目录也不会去动它；
//   - 平台 ID（OpenQQ-Group:...）与控制台会话键（group:<group_openid>）互相独立，
//     不参与身份绑定归一，避免把绑定逻辑卷进来；
//   - 官方平台无法回溯历史，只能从功能启用后开始记录。

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	chatConsoleIndexName = "index.json"

	// 聊天控制台保留策略：按天滚动，默认保留 30 天；单次历史查询上限 200 条。
	chatConsoleRetentionDays = 30
	chatConsoleMaxTextLen    = 8000
	chatConsolePreviewLen    = 80
	chatConsoleDedupeWindow  = 2000
	chatConsolePageLimit     = 50
	chatConsolePageMax       = 200
	chatConsolePruneInterval = time.Hour
)

// 官方 QQ 会话键。控制台只认这两个前缀，和 dice 内部的 OpenQQ-Group / OpenQQ 不同。
const (
	chatConsoleKindGroup = "group"
	chatConsoleKindC2C   = "c2c"

	chatConsoleDirIn  = "in"
	chatConsoleDirOut = "out"

	chatConsoleSourceUser    = "user"
	chatConsoleSourceBot     = "bot"
	chatConsoleSourceConsole = "console"
)

// chatConsoleCQRe 匹配官方适配器写进正文的附件 CQ 码：[CQ:image,file=URL]。
var chatConsoleCQRe = regexp.MustCompile(`\[CQ:(image|record|video|file),file=([^,\]]+)\]`)

// ChatConsoleMedia 是消息里的一个媒体附件（只存元信息，不落原始文件）。
type ChatConsoleMedia struct {
	Kind string `json:"kind"`           // image / record / video / file
	URL  string `json:"url"`            // 官方临时链接，过期后前端显示"已过期"
	Name string `json:"name,omitempty"` // 文件名（file 才有）
}

// ChatConsoleQuote 是骰主在控制台里"引用回复"的引用信息。
// 官方主动消息挂不了 msg_id，所以这只是展示层面的引用块 + 正文前缀。
type ChatConsoleQuote struct {
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

// ChatConsoleMessage 一条消息。Text 保留原始文本（可能含 CQ 码），
// Media 是从文本里解析出来的附件，方便前端直接渲染。
type ChatConsoleMessage struct {
	ID         string `json:"id"`
	TS         int64  `json:"ts"`
	Dir        string `json:"dir"`    // in / out
	Source     string `json:"source"` // user / bot / console
	SenderID   string `json:"senderId"`
	SenderName string `json:"senderName"`
	// SenderOpenID 是官方侧的 member_openid / user_openid，禁言等功能需要它。
	SenderOpenID string             `json:"senderOpenId,omitempty"`
	Text         string             `json:"text"`
	Media        []ChatConsoleMedia `json:"media,omitempty"`
	MsgID        string             `json:"msgId,omitempty"`
	Quote        *ChatConsoleQuote  `json:"quote,omitempty"`
	// Recalled 表示骰主已经在控制台里撤回了这条消息。
	Recalled   bool  `json:"recalled,omitempty"`
	RecalledAt int64 `json:"recalledAt,omitempty"`
}

// ChatConsoleConversation 会话元信息。Name 可能是官方返回的群名，也可能是
// UI 上自动生成的占位名（NameManual 标记是否为骰主手动改名）。
type ChatConsoleConversation struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"` // group / c2c
	TargetID    string `json:"targetId"`
	UIN         string `json:"uin,omitempty"`
	EndpointID  string `json:"endpointId,omitempty"`
	Name        string `json:"name"`
	NameManual  bool   `json:"nameManual"`
	MemberCount int    `json:"memberCount,omitempty"`
	// FullMessage 表示该群已在官方群设置里授权"获取全部群消息"。
	// 目前只能靠"是否收到过非 @ 群消息"推断，未授权时 UI 需提示。
	FullMessage bool   `json:"fullMessage"`
	LastTS      int64  `json:"lastTs"`
	LastText    string `json:"lastText"`
	Unread      int    `json:"unread"`
	UpdatedAt   int64  `json:"updatedAt"`
}

// ChatConsoleEvent 是推给 WebUI 的实时事件（SSE）。
type ChatConsoleEvent struct {
	Type         string                   `json:"type"` // message / conversation
	Conversation *ChatConsoleConversation `json:"conversation,omitempty"`
	Message      *ChatConsoleMessage      `json:"message,omitempty"`
}

// ChatConsoleRecord 是采集端交给存储层的一条新消息。
type ChatConsoleRecord struct {
	Kind         string // group / c2c
	TargetID     string // group_openid / user_openid
	UIN          string
	EndpointID   string
	SenderID     string
	SenderName   string
	SenderOpenID string
	Dir          string // in / out
	Source       string // user / bot / console
	Text         string
	MessageID    string // 官方 msg_id，用于去重
	TS           int64
	FullMessage  bool              // 该群已授权全量消息
	Quote        *ChatConsoleQuote // 控制台引用回复的引用信息
}

type chatConsoleIndex struct {
	Version       int                                 `json:"version"`
	Conversations map[string]*ChatConsoleConversation `json:"conversations"`
}

// ChatConsoleStore 管理 chat-console 目录下的会话索引与消息文件。
// 所有方法并发安全；索引在每次写入后立即原子落盘（文件很小）。
type ChatConsoleStore struct {
	dir string

	mu        sync.Mutex
	loaded    bool
	convs     map[string]*ChatConsoleConversation
	seen      map[string]map[string]struct{} // 会话 -> 最近的官方 msg_id
	seenSeq   map[string][]string            // 与 seen 配套的写入顺序，用于滑动窗口
	lastPrune time.Time

	subMu  sync.Mutex
	subs   map[int]func(ChatConsoleEvent)
	nextID int
}

// NewChatConsoleStore 创建一个存储实例。dir 通常是 <dataDir>/chat-console。
func NewChatConsoleStore(dir string) *ChatConsoleStore {
	return &ChatConsoleStore{
		dir:     dir,
		convs:   map[string]*ChatConsoleConversation{},
		seen:    map[string]map[string]struct{}{},
		seenSeq: map[string][]string{},
		subs:    map[int]func(ChatConsoleEvent){},
	}
}

// Dir 返回数据目录（测试与文档用）。
func (s *ChatConsoleStore) Dir() string { return s.dir }

// GetChatConsole 懒加载聊天控制台存储；未配置 DataDir 时退化到 data/chat-console。
func (d *Dice) GetChatConsole() *ChatConsoleStore {
	if d == nil {
		return nil
	}
	d.chatConsoleOnce.Do(func() {
		dir := filepath.Join("data", "chat-console")
		if d.BaseConfig.DataDir != "" {
			dir = filepath.Join(d.BaseConfig.DataDir, "chat-console")
		}
		d.chatConsole = NewChatConsoleStore(dir)
	})
	return d.chatConsole
}

// ---------------- 内部：路径与序列化 ----------------

func chatConsoleSafeName(key string) string {
	var b strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	name := b.String()
	if name == "" {
		name = "unknown"
	}
	return name
}

func chatConsoleDay(ts int64) string {
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	return time.Unix(ts, 0).In(officialQQTimeZone).Format("2006-01-02")
}

func (s *ChatConsoleStore) conversationDir(key string) string {
	return filepath.Join(s.dir, chatConsoleSafeName(key))
}

func (s *ChatConsoleStore) dayFile(key string, ts int64) string {
	return filepath.Join(s.conversationDir(key), chatConsoleDay(ts)+".jsonl")
}

func (s *ChatConsoleStore) indexFile() string {
	return filepath.Join(s.dir, chatConsoleIndexName)
}

// ensureLoaded 首次访问时读入索引并顺带清理过期日文件。调用方需持有 s.mu。
func (s *ChatConsoleStore) ensureLoaded() {
	if s.loaded {
		return
	}
	s.loaded = true

	raw, err := os.ReadFile(s.indexFile())
	if err == nil {
		var idx chatConsoleIndex
		if err := json.Unmarshal(raw, &idx); err == nil && idx.Conversations != nil {
			for k, v := range idx.Conversations {
				if v == nil {
					continue
				}
				v.Key = k
				s.convs[k] = v
			}
		}
	}
	s.pruneLocked()
}

// writeFileAtomic 先写临时文件再 rename，避免读到写了一半的日文件。
// path 由 conversationDir（已做字符白名单过滤）+ 日期文件名拼成，不来自外部输入。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // 路径来自内部拼接
		return err
	}
	return os.Rename(tmp, path)
}

func (s *ChatConsoleStore) saveIndexLocked() {
	idx := chatConsoleIndex{
		Version:       1,
		Conversations: s.convs,
	}
	raw, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return
	}
	tmp := s.indexFile() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.indexFile())
}

// pruneLocked 删除超过保留期的日文件。会话本身保留（历史被删，但会话仍可继续
// 使用），这样骰主不会因为清理丢掉群名、权限状态等元信息。
func (s *ChatConsoleStore) pruneLocked() {
	now := time.Now()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < chatConsolePruneInterval {
		return
	}
	s.lastPrune = now

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	cutoff := now.In(officialQQTimeZone).AddDate(0, 0, -chatConsoleRetentionDays).Format("2006-01-02")
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		convDir := filepath.Join(s.dir, entry.Name())
		files, err := os.ReadDir(convDir)
		if err != nil {
			continue
		}
		remaining := 0
		for _, f := range files {
			name := f.Name()
			if !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			day := strings.TrimSuffix(name, ".jsonl")
			if day < cutoff {
				_ = os.Remove(filepath.Join(convDir, name))
				continue
			}
			remaining++
		}
		if remaining == 0 {
			_ = os.Remove(convDir)
		}
	}
}

// ---------------- 内部：媒体解析与预览 ----------------

func parseChatConsoleMedia(text string) []ChatConsoleMedia {
	if !strings.Contains(text, "[CQ:") {
		return nil
	}
	var out []ChatConsoleMedia
	for _, m := range chatConsoleCQRe.FindAllStringSubmatch(text, -1) {
		kind, rawURL := m[1], m[2]
		if rawURL == "" {
			continue
		}
		item := ChatConsoleMedia{Kind: kind, URL: rawURL}
		if kind == "file" {
			name := rawURL
			if i := strings.Index(name, "?"); i > 0 {
				name = name[:i]
			}
			item.Name = filepath.Base(strings.TrimRight(name, "/"))
		}
		out = append(out, item)
	}
	return out
}

func chatConsolePreview(text string) string {
	plain := chatConsoleCQRe.ReplaceAllString(text, "")
	plain = strings.Join(strings.Fields(plain), " ")
	runes := []rune(plain)
	if len(runes) > chatConsolePreviewLen {
		return string(runes[:chatConsolePreviewLen]) + "…"
	}
	return plain
}

func chatConsoleShortID(id string) string {
	if len(id) <= 6 {
		return id
	}
	return id[len(id)-6:]
}

func chatConsoleDefaultName(kind, targetID string) string {
	short := chatConsoleShortID(targetID)
	if kind == chatConsoleKindGroup {
		return "群 " + short
	}
	return "私聊 " + short
}

// ---------------- 公共 API：写入 ----------------

func (s *ChatConsoleStore) conversationKey(kind, targetID string) string {
	return kind + ":" + targetID
}

// Append 写入一条消息，返回落盘后的消息副本。MessageID 相同的消息会被丢弃
// （官方网关可能重投），此时返回 nil, nil。
func (s *ChatConsoleStore) Append(rec ChatConsoleRecord) (*ChatConsoleMessage, error) {
	if rec.Kind != chatConsoleKindGroup && rec.Kind != chatConsoleKindC2C {
		return nil, fmt.Errorf("未知的会话类型: %s", rec.Kind)
	}
	if rec.TargetID == "" {
		return nil, errors.New("会话目标 ID 为空")
	}
	if rec.TS <= 0 {
		rec.TS = time.Now().Unix()
	}
	if rec.Dir == "" {
		rec.Dir = chatConsoleDirIn
	}
	if rec.Source == "" {
		rec.Source = chatConsoleSourceUser
	}

	text := rec.Text
	if runes := []rune(text); len(runes) > chatConsoleMaxTextLen {
		text = string(runes[:chatConsoleMaxTextLen]) + "…"
	}

	key := s.conversationKey(rec.Kind, rec.TargetID)

	s.mu.Lock()
	s.ensureLoaded()

	if rec.MessageID != "" {
		if set, ok := s.seen[key]; ok {
			if _, dup := set[rec.MessageID]; dup {
				s.mu.Unlock()
				return nil, nil //nolint:nilnil // 重复消息不是错误：返回 nil 表示已被去重丢弃
			}
		}
	}

	msg := &ChatConsoleMessage{
		ID:           rec.MessageID,
		TS:           rec.TS,
		Dir:          rec.Dir,
		Source:       rec.Source,
		SenderID:     rec.SenderID,
		SenderName:   rec.SenderName,
		SenderOpenID: rec.SenderOpenID,
		Text:         text,
		Media:        parseChatConsoleMedia(text),
		MsgID:        rec.MessageID,
		Quote:        rec.Quote,
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}

	if err := s.appendLineLocked(key, msg); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.rememberLocked(key, rec.MessageID)

	conv, ok := s.convs[key]
	if !ok {
		conv = &ChatConsoleConversation{
			Key:        key,
			Kind:       rec.Kind,
			TargetID:   rec.TargetID,
			UIN:        rec.UIN,
			EndpointID: rec.EndpointID,
			Name:       chatConsoleDefaultName(rec.Kind, rec.TargetID),
		}
		s.convs[key] = conv
	}
	if rec.UIN != "" {
		conv.UIN = rec.UIN
	}
	if rec.EndpointID != "" {
		conv.EndpointID = rec.EndpointID
	}
	if rec.FullMessage {
		conv.FullMessage = true
	}
	if rec.SenderName != "" && rec.Kind == chatConsoleKindC2C && conv.Name == chatConsoleDefaultName(conv.Kind, conv.TargetID) {
		conv.Name = rec.SenderName
	}
	conv.LastTS = msg.TS
	conv.LastText = chatConsolePreview(msg.Text)
	conv.UpdatedAt = time.Now().Unix()
	if msg.Dir == chatConsoleDirIn {
		conv.Unread++
	}

	s.pruneLocked()
	s.saveIndexLocked()

	convCopy := *conv
	msgCopy := *msg
	s.mu.Unlock()

	s.publish(ChatConsoleEvent{Type: "message", Conversation: &convCopy, Message: &msgCopy})
	return &msgCopy, nil
}

func (s *ChatConsoleStore) appendLineLocked(key string, msg *ChatConsoleMessage) error {
	file := s.dayFile(key, msg.TS)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func (s *ChatConsoleStore) rememberLocked(key, msgID string) {
	if msgID == "" {
		return
	}
	set, ok := s.seen[key]
	if !ok {
		set = map[string]struct{}{}
		s.seen[key] = set
	}
	set[msgID] = struct{}{}
	seq := s.seenSeq[key]
	seq = append(seq, msgID)
	if len(seq) > chatConsoleDedupeWindow {
		drop := seq[:len(seq)-chatConsoleDedupeWindow]
		for _, id := range drop {
			delete(set, id)
		}
		seq = seq[len(seq)-chatConsoleDedupeWindow:]
	}
	s.seenSeq[key] = seq
}

// ---------------- 公共 API：读取 ----------------

// Conversations 返回按最后消息时间倒序排列的会话副本。
func (s *ChatConsoleStore) Conversations() []*ChatConsoleConversation {
	s.mu.Lock()
	s.ensureLoaded()
	out := make([]*ChatConsoleConversation, 0, len(s.convs))
	for _, conv := range s.convs {
		if conv.LastTS == 0 {
			continue
		}
		c := *conv
		out = append(out, &c)
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].LastTS == out[j].LastTS {
			return out[i].Key < out[j].Key
		}
		return out[i].LastTS > out[j].LastTS
	})
	return out
}

// Conversation 返回单个会话副本，不存在时返回 nil。
func (s *ChatConsoleStore) Conversation(key string) *ChatConsoleConversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()
	conv, ok := s.convs[key]
	if !ok {
		return nil
	}
	c := *conv
	return &c
}

// Messages 向前分页读取历史：before 为 0 表示从最新开始，返回按时间升序的切片。
// hasMore 表示更早的位置可能还有消息。
func (s *ChatConsoleStore) Messages(key string, before int64, limit int) ([]ChatConsoleMessage, bool, error) {
	if limit <= 0 {
		limit = chatConsolePageLimit
	}
	if limit > chatConsolePageMax {
		limit = chatConsolePageMax
	}
	dir := s.conversationDir(key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []ChatConsoleMessage{}, false, nil
		}
		return nil, false, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	var collected []ChatConsoleMessage
	hasMore := false
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var msg ChatConsoleMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if before > 0 && msg.TS >= before {
				continue
			}
			if len(collected) >= limit {
				hasMore = true
				break
			}
			collected = append(collected, msg)
		}
		if hasMore {
			break
		}
	}
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	if collected == nil {
		collected = []ChatConsoleMessage{}
	}
	return collected, hasMore, nil
}

// MarkRead 清空未读数。
func (s *ChatConsoleStore) MarkRead(key string) *ChatConsoleConversation {
	s.mu.Lock()
	s.ensureLoaded()
	conv, ok := s.convs[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	conv.Unread = 0
	c := *conv
	s.saveIndexLocked()
	s.mu.Unlock()

	s.publish(ChatConsoleEvent{Type: "conversation", Conversation: &c})
	return &c
}

// Rename 手动设置会话显示名。
func (s *ChatConsoleStore) Rename(key, name string) (*ChatConsoleConversation, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("名称不能为空")
	}
	if runes := []rune(name); len(runes) > 40 {
		name = string(runes[:40])
	}

	s.mu.Lock()
	s.ensureLoaded()
	conv, ok := s.convs[key]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("会话不存在")
	}
	conv.Name = name
	conv.NameManual = true
	conv.UpdatedAt = time.Now().Unix()
	c := *conv
	s.saveIndexLocked()
	s.mu.Unlock()

	s.publish(ChatConsoleEvent{Type: "conversation", Conversation: &c})
	return &c, nil
}

// findMessageLocked 在会话的日文件里按 ID 倒序查找一条消息，返回所在文件、行号与副本。
func (s *ChatConsoleStore) findMessageLocked(key, msgID string) (string, int, *ChatConsoleMessage) {
	if msgID == "" {
		return "", -1, nil
	}
	dir := s.conversationDir(key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", -1, nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))

	for _, name := range names {
		file := filepath.Join(dir, name)
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var msg ChatConsoleMessage
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			if msg.ID != msgID && msg.MsgID != msgID {
				continue
			}
			msgCopy := msg
			return file, i, &msgCopy
		}
	}
	return "", -1, nil
}

// Find 按 ID 查询一条消息（不存在时返回 nil）。
func (s *ChatConsoleStore) Find(key, msgID string) *ChatConsoleMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()
	_, _, msg := s.findMessageLocked(key, msgID)
	return msg
}

// MarkRecalled 把一条已发送的消息标记为"已撤回"：改写所在日文件并推送事件。
// msgID 可以传官方 msg_id，也可以传控制台内部 ID。
func (s *ChatConsoleStore) MarkRecalled(key, msgID string) (*ChatConsoleMessage, error) {
	if msgID == "" {
		return nil, errors.New("消息 ID 为空")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureLoaded()

	file, lineIdx, hit := s.findMessageLocked(key, msgID)
	if hit == nil {
		return nil, errors.New("没有找到这条消息（可能已被清理）")
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	if lineIdx < 0 || lineIdx >= len(lines) {
		return nil, errors.New("消息文件已变化，请刷新后重试")
	}
	now := time.Now().Unix()
	hit.Recalled = true
	hit.RecalledAt = now
	recalled, err := json.Marshal(hit)
	if err != nil {
		return nil, err
	}
	lines[lineIdx] = string(recalled)
	if err := writeFileAtomic(file, []byte(strings.Join(lines, "\n"))); err != nil {
		return nil, err
	}

	conv, ok := s.convs[key]
	if ok && conv.LastTS == hit.TS {
		// 最后一条被撤回时，列表预览也跟着变成"已撤回"
		conv.LastText = "（消息已撤回）"
	}
	s.saveIndexLocked()

	var convCopy *ChatConsoleConversation
	if ok {
		c := *conv
		convCopy = &c
	}
	hitCopy := *hit
	s.publish(ChatConsoleEvent{Type: "recall", Conversation: convCopy, Message: &hitCopy})
	return &hitCopy, nil
}

// SetGroupInfo 写入官方返回的群名 / 成员数。手动改过名的会话不会被覆盖。
func (s *ChatConsoleStore) SetGroupInfo(key, name string, memberCount int) *ChatConsoleConversation {
	s.mu.Lock()
	s.ensureLoaded()
	conv, ok := s.convs[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	changed := false
	if name != "" && !conv.NameManual {
		conv.Name = name
		changed = true
	}
	if memberCount > 0 && conv.MemberCount != memberCount {
		conv.MemberCount = memberCount
		changed = true
	}
	if !changed {
		s.mu.Unlock()
		return nil
	}
	c := *conv
	s.saveIndexLocked()
	s.mu.Unlock()

	s.publish(ChatConsoleEvent{Type: "conversation", Conversation: &c})
	return &c
}

// ---------------- 公共 API：实时订阅 ----------------

// Subscribe 注册事件回调，返回取消函数。回调在独立 goroutine 里执行。
func (s *ChatConsoleStore) Subscribe(fn func(ChatConsoleEvent)) func() {
	if fn == nil {
		return func() {}
	}
	s.subMu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = fn
	s.subMu.Unlock()

	return func() {
		s.subMu.Lock()
		delete(s.subs, id)
		s.subMu.Unlock()
	}
}

func (s *ChatConsoleStore) publish(event ChatConsoleEvent) {
	s.subMu.Lock()
	subs := make([]func(ChatConsoleEvent), 0, len(s.subs))
	for _, fn := range s.subs {
		subs = append(subs, fn)
	}
	s.subMu.Unlock()

	for _, fn := range subs {
		go func(f func(ChatConsoleEvent)) {
			defer func() { _ = recover() }()
			f(event)
		}(fn)
	}
}
