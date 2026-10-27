package dice

// 官方 QQ 聊天控制台：骰主在 WebUI 里以机器人身份查看 / 发送消息。
//
// 存储方案（v1.2 起）：<dataDir>/chat-console/console.db（独立的 SQLite 文件）。
//   - 只在本功能自己的目录里建库，**不动** data.db，也不改任何既有 schema；
//   - 删掉 chat-console/ 目录 = 清空控制台，旧版本程序不认识这个目录；
//   - 分页 / 按 msg_id 查找 / 撤回 / 按天删除都有索引，500 个群也能用；
//   - 老版本的 index.json + 按天 jsonl 会在首次启动时自动导入，原始文件被挪到
//     legacy-jsonl/ 保留一份，确认无误后可自行删除。

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

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	glogger "gorm.io/gorm/logger"
)

const (
	chatConsoleDBName       = "console.db"
	chatConsoleSettingsName = "settings.json"
	chatConsoleLegacyDir    = "legacy-jsonl"

	// 聊天控制台保留策略：按天滚动，天数可在 WebUI 里调整（0 = 永久保留）。
	chatConsoleDefaultRetentionDays = 30
	chatConsoleMaxRetentionDays     = 3650
	chatConsoleMaxTextLen           = 8000
	chatConsolePreviewLen           = 80
	chatConsolePageLimit            = 50
	chatConsolePageMax              = 200
	chatConsolePruneInterval        = time.Hour
	// 存储占用扫描结果缓存（聚合查询 + 目录扫描都不便宜）。
	chatConsoleUsageCacheTTL = 15 * time.Second
	// 单条消息在 SQLite 里的固定开销估算（列头、索引项、行头），只用于占用展示。
	chatConsoleRowOverheadBytes = 160
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
	SenderOpenID string `json:"senderOpenId,omitempty"`
	// AvatarURL 由 AppID + openid 拼出来的头像地址（读的时候计算，不落库）。
	AvatarURL string             `json:"avatarUrl,omitempty"`
	Text      string             `json:"text"`
	Media     []ChatConsoleMedia `json:"media,omitempty"`
	MsgID     string             `json:"msgId,omitempty"`
	Quote     *ChatConsoleQuote  `json:"quote,omitempty"`
	// Recalled 表示骰主已经在控制台里撤回了这条消息。
	Recalled   bool  `json:"recalled,omitempty"`
	RecalledAt int64 `json:"recalledAt,omitempty"`
}

// ChatConsoleConversation 会话元信息。Name 可能是官方返回的群名，也可能是
// UI 上自动生成的占位名（NameManual 标记是否为骰主手动改名）。
type ChatConsoleConversation struct {
	Key        string `json:"key"`
	Kind       string `json:"kind"` // group / c2c
	TargetID   string `json:"targetId"`
	UIN        string `json:"uin,omitempty"`
	EndpointID string `json:"endpointId,omitempty"`
	// AppID 用来拼头像地址（https://thirdqq.qlogo.cn/qqapp/{appID}/{openid}/0）。
	AppID       string `json:"-"`
	AvatarURL   string `json:"avatarUrl,omitempty"`
	Name        string `json:"name"`
	NameManual  bool   `json:"nameManual"`
	MemberCount int    `json:"memberCount,omitempty"`
	// FullMessage 表示该群已在官方群设置里授权"获取全部群消息"。
	// 目前只能靠"是否收到过非 @ 群消息"推断，未授权时 UI 需提示。
	FullMessage bool `json:"fullMessage"`
	// Pinned 表示骰主把该会话置顶了（置顶排前面，不受新消息影响）。
	Pinned    bool   `json:"pinned"`
	LastTS    int64  `json:"lastTs"`
	LastText  string `json:"lastText"`
	Unread    int    `json:"unread"`
	UpdatedAt int64  `json:"updatedAt"`
}

// ChatConsoleEvent 是推给 WebUI 的实时事件（SSE）。
type ChatConsoleEvent struct {
	Type         string                   `json:"type"` // message / recall / conversation / settings
	Conversation *ChatConsoleConversation `json:"conversation,omitempty"`
	Message      *ChatConsoleMessage      `json:"message,omitempty"`
}

// ChatConsoleRecord 是采集端交给存储层的一条新消息。
type ChatConsoleRecord struct {
	Kind         string // group / c2c
	TargetID     string // group_openid / user_openid
	UIN          string
	EndpointID   string
	AppID        string
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

// ChatConsoleUsage 单个会话的磁盘占用（SQLite 里按列长度估算）。
type ChatConsoleUsage struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Bytes    int64  `json:"bytes"`
	Messages int    `json:"messages"`
	Days     int    `json:"days"`
	OldestTS int64  `json:"oldestTs,omitempty"`
	NewestTS int64  `json:"newestTs,omitempty"`
}

// chatConsoleSettings 是控制台自己的设置文件（不进数据库、不进 serve.yaml）。
type chatConsoleSettings struct {
	// RetentionDays 保留天数；0 表示永久保留。
	RetentionDays int `json:"retentionDays"`
}

// chatConsoleConversationRow / chatConsoleMessageRow 是 SQLite 表结构。
type chatConsoleConversationRow struct {
	Key         string `gorm:"column:key;primaryKey"`
	Kind        string `gorm:"column:kind;not null"`
	TargetID    string `gorm:"column:target_id;not null"`
	UIN         string `gorm:"column:uin"`
	EndpointID  string `gorm:"column:endpoint_id"`
	AppID       string `gorm:"column:app_id"`
	Name        string `gorm:"column:name"`
	NameManual  bool   `gorm:"column:name_manual;not null;default:false"`
	MemberCount int    `gorm:"column:member_count;not null;default:0"`
	FullMessage bool   `gorm:"column:full_message;not null;default:false"`
	Pinned      bool   `gorm:"column:pinned;not null;default:false"`
	LastTS      int64  `gorm:"column:last_ts;not null;default:0"`
	LastText    string `gorm:"column:last_text"`
	Unread      int    `gorm:"column:unread;not null;default:0"`
	UpdatedAt   int64  `gorm:"column:updated_at;not null;default:0"`
}

func (chatConsoleConversationRow) TableName() string { return "chat_console_conversations" }

type chatConsoleMessageRow struct {
	ID           string `gorm:"column:id;primaryKey"`
	MsgID        string `gorm:"column:msg_id;index:idx_chat_msgs_msgid"`
	ConvKey      string `gorm:"column:conv_key;not null;index:idx_chat_msgs_conv_ts,priority:1"`
	TS           int64  `gorm:"column:ts;not null;index:idx_chat_msgs_conv_ts,priority:2"`
	Dir          string `gorm:"column:dir;not null"`
	Source       string `gorm:"column:source;not null"`
	SenderID     string `gorm:"column:sender_id"`
	SenderName   string `gorm:"column:sender_name"`
	SenderOpenID string `gorm:"column:sender_open_id"`
	Text         string `gorm:"column:text"`
	Media        string `gorm:"column:media"`
	Quote        string `gorm:"column:quote"`
	Recalled     bool   `gorm:"column:recalled;not null;default:false"`
	RecalledAt   int64  `gorm:"column:recalled_at;not null;default:0"`
}

func (chatConsoleMessageRow) TableName() string { return "chat_console_messages" }

// ChatConsoleStore 管理 chat-console 目录下的 SQLite 库与设置。
// 所有方法并发安全。
type ChatConsoleStore struct {
	dir string

	mu             sync.Mutex
	opened         bool
	db             *gorm.DB
	retentionDays  int
	settingsLoaded bool
	lastPrune      time.Time
	usageCache     *chatConsoleUsageCache

	subMu  sync.Mutex
	subs   map[int]func(ChatConsoleEvent)
	nextID int
}

// NewChatConsoleStore 创建一个存储实例。dir 通常是 <dataDir>/chat-console。
// 真正的建库发生在第一次使用时（懒加载）。
func NewChatConsoleStore(dir string) *ChatConsoleStore {
	return &ChatConsoleStore{
		dir:  dir,
		subs: map[int]func(ChatConsoleEvent){},
	}
}

// Dir 返回数据目录（测试与文档用）。
func (s *ChatConsoleStore) Dir() string { return s.dir }

// Close 关闭数据库连接（测试与优雅关停时用）。关闭后再次使用会重新打开。
func (s *ChatConsoleStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return
	}
	if sqlDB, err := s.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	s.db = nil
	s.opened = false
}

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

// ---------------- 内部：建库与路径 ----------------

func (s *ChatConsoleStore) dbFile() string {
	return filepath.Join(s.dir, chatConsoleDBName)
}

func (s *ChatConsoleStore) legacyDir() string {
	return filepath.Join(s.dir, chatConsoleLegacyDir)
}

// openLocked 打开数据库、建表、迁移老数据并清理过期记录。调用方需持有 s.mu。
func (s *ChatConsoleStore) openLocked() error {
	if s.opened {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}

	dsn := fmt.Sprintf("file:%v?_txlock=immediate&_busy_timeout=15000", s.dbFile())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		// 控制台自己的查询不写进海豹主日志，免得"会话不存在"这类正常分支刷屏
		Logger: glogger.Default.LogMode(glogger.Silent),
	})
	if err != nil {
		return err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	// 单连接：写入串行，彻底避开 database is locked；本功能并发压力很小。
	sqlDB.SetMaxOpenConns(1)
	_ = db.Exec("PRAGMA journal_mode=WAL").Error
	_ = db.Exec("PRAGMA synchronous=NORMAL").Error
	_ = db.Exec("PRAGMA busy_timeout=15000").Error

	if err := db.AutoMigrate(&chatConsoleConversationRow{}, &chatConsoleMessageRow{}); err != nil {
		return err
	}
	// 官方 msg_id 去重：只对非空 msg_id 建唯一索引（本地生成的 uuid 不参与）。
	if err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_chat_msgs_msgid_uniq
		ON chat_console_messages(msg_id) WHERE msg_id <> ''`).Error; err != nil {
		return err
	}

	s.db = db
	s.opened = true
	s.loadSettingsLocked()
	s.migrateLegacyJSONLLocked()
	s.pruneLocked()
	return nil
}

func (s *ChatConsoleStore) loadSettingsLocked() {
	if s.settingsLoaded {
		return
	}
	s.settingsLoaded = true
	raw, err := os.ReadFile(filepath.Join(s.dir, chatConsoleSettingsName)) //nolint:gosec // 内部路径
	if err == nil {
		var cfg chatConsoleSettings
		if err := json.Unmarshal(raw, &cfg); err == nil {
			s.retentionDays = clampChatConsoleRetention(cfg.RetentionDays)
			return
		}
	}
	s.retentionDays = chatConsoleDefaultRetentionDays
}

func clampChatConsoleRetention(days int) int {
	if days < 0 {
		return 0
	}
	if days > chatConsoleMaxRetentionDays {
		return chatConsoleMaxRetentionDays
	}
	return days
}

func (s *ChatConsoleStore) saveSettingsLocked() {
	raw, err := json.MarshalIndent(chatConsoleSettings{RetentionDays: s.retentionDays}, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(s.dir, 0o755)
	_ = writeFileAtomic(filepath.Join(s.dir, chatConsoleSettingsName), raw)
}

// writeFileAtomic 先写临时文件再 rename，避免读到写了一半的文件。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // 路径来自内部拼接
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------- 内部：行与结构体互转 ----------------

func chatConsoleMediaJSON(media []ChatConsoleMedia) string {
	if len(media) == 0 {
		return ""
	}
	raw, err := json.Marshal(media)
	if err != nil {
		return ""
	}
	return string(raw)
}

func chatConsoleQuoteJSON(quote *ChatConsoleQuote) string {
	if quote == nil {
		return ""
	}
	raw, err := json.Marshal(quote)
	if err != nil {
		return ""
	}
	return string(raw)
}

func chatConsoleRowToMessage(row chatConsoleMessageRow) ChatConsoleMessage {
	msg := ChatConsoleMessage{
		ID:           row.ID,
		MsgID:        row.MsgID,
		TS:           row.TS,
		Dir:          row.Dir,
		Source:       row.Source,
		SenderID:     row.SenderID,
		SenderName:   row.SenderName,
		SenderOpenID: row.SenderOpenID,
		Text:         row.Text,
		Recalled:     row.Recalled,
		RecalledAt:   row.RecalledAt,
	}
	if row.Media != "" {
		_ = json.Unmarshal([]byte(row.Media), &msg.Media)
	}
	if row.Quote != "" {
		var quote ChatConsoleQuote
		if err := json.Unmarshal([]byte(row.Quote), &quote); err == nil {
			msg.Quote = &quote
		}
	}
	return msg
}

func chatConsoleConvToRow(conv *ChatConsoleConversation) chatConsoleConversationRow {
	return chatConsoleConversationRow{
		Key:         conv.Key,
		Kind:        conv.Kind,
		TargetID:    conv.TargetID,
		UIN:         conv.UIN,
		EndpointID:  conv.EndpointID,
		AppID:       conv.AppID,
		Name:        conv.Name,
		NameManual:  conv.NameManual,
		MemberCount: conv.MemberCount,
		FullMessage: conv.FullMessage,
		Pinned:      conv.Pinned,
		LastTS:      conv.LastTS,
		LastText:    conv.LastText,
		Unread:      conv.Unread,
		UpdatedAt:   conv.UpdatedAt,
	}
}

func chatConsoleRowToConv(row chatConsoleConversationRow) *ChatConsoleConversation {
	conv := &ChatConsoleConversation{
		Key:         row.Key,
		Kind:        row.Kind,
		TargetID:    row.TargetID,
		UIN:         row.UIN,
		EndpointID:  row.EndpointID,
		AppID:       row.AppID,
		Name:        row.Name,
		NameManual:  row.NameManual,
		MemberCount: row.MemberCount,
		FullMessage: row.FullMessage,
		Pinned:      row.Pinned,
		LastTS:      row.LastTS,
		LastText:    row.LastText,
		Unread:      row.Unread,
		UpdatedAt:   row.UpdatedAt,
	}
	// 群头像需要群号（官方只给 group_openid），拼不出来；私聊可以直接用 openid
	if conv.Kind == chatConsoleKindC2C {
		conv.AvatarURL = chatConsoleAvatarURL(row.AppID, row.TargetID)
	}
	return conv
}

// chatConsoleAvatarURL 官方 bot 的用户头像：
// https://thirdqq.qlogo.cn/qqapp/{appID}/{openid}/0
func chatConsoleAvatarURL(appID, openID string) string {
	if appID == "" || openID == "" {
		return ""
	}
	return fmt.Sprintf("https://thirdqq.qlogo.cn/qqapp/%s/%s/0", appID, openID)
}

// ---------------- 内部：文本小工具 ----------------

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

// chatConsoleDay 把时间戳格式化成 UTC+8 的日期（迁移老 jsonl 时按天找文件名）。
func chatConsoleDay(ts int64) string {
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	return time.Unix(ts, 0).In(officialQQTimeZone).Format("2006-01-02")
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
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()

	msg := ChatConsoleMessage{
		ID:           rec.MessageID,
		MsgID:        rec.MessageID,
		TS:           rec.TS,
		Dir:          rec.Dir,
		Source:       rec.Source,
		SenderID:     rec.SenderID,
		SenderName:   rec.SenderName,
		SenderOpenID: rec.SenderOpenID,
		Text:         text,
		Media:        parseChatConsoleMedia(text),
		Quote:        rec.Quote,
	}
	if msg.ID == "" {
		msg.ID = uuid.NewString()
	}

	row := chatConsoleMessageRow{
		ID:           msg.ID,
		MsgID:        msg.MsgID,
		ConvKey:      key,
		TS:           msg.TS,
		Dir:          msg.Dir,
		Source:       msg.Source,
		SenderID:     msg.SenderID,
		SenderName:   msg.SenderName,
		SenderOpenID: msg.SenderOpenID,
		Text:         msg.Text,
		Media:        chatConsoleMediaJSON(msg.Media),
		Quote:        chatConsoleQuoteJSON(msg.Quote),
	}

	res := s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		// 重复投递：官方 msg_id 撞唯一索引
		return nil, nil //nolint:nilnil // 重复消息不是错误：返回 nil 表示已被去重丢弃
	}

	conv := s.upsertConversation(rec, key, msg)
	s.pruneIfNeeded()
	s.invalidateUsageLocked()

	convCopy := *conv
	msgCopy := msg
	s.publish(ChatConsoleEvent{Type: "message", Conversation: &convCopy, Message: &msgCopy})
	return &msgCopy, nil
}

// upsertConversation 更新会话元信息并返回最新副本。
func (s *ChatConsoleStore) upsertConversation(rec ChatConsoleRecord, key string, msg ChatConsoleMessage) *ChatConsoleConversation {
	s.mu.Lock()
	defer s.mu.Unlock()

	var row chatConsoleConversationRow
	err := s.db.Where("key = ?", key).Take(&row).Error
	now := time.Now().Unix()
	if err != nil {
		row = chatConsoleConversationRow{
			Key:        key,
			Kind:       rec.Kind,
			TargetID:   rec.TargetID,
			UIN:        rec.UIN,
			EndpointID: rec.EndpointID,
			AppID:      rec.AppID,
			Name:       chatConsoleDefaultName(rec.Kind, rec.TargetID),
			LastTS:     msg.TS,
			LastText:   chatConsolePreview(msg.Text),
			UpdatedAt:  now,
		}
		if msg.Dir == chatConsoleDirIn {
			row.Unread = 1
		}
		conv := chatConsoleRowToConv(row)
		if rec.SenderName != "" && rec.Kind == chatConsoleKindC2C && conv.Name == chatConsoleDefaultName(conv.Kind, conv.TargetID) {
			conv.Name = rec.SenderName
			conv.NameManual = false
		}
		if rec.FullMessage {
			conv.FullMessage = true
		}
		convRow := chatConsoleConvToRow(conv)
		s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&convRow)
		return conv
	}

	conv := chatConsoleRowToConv(row)
	if rec.UIN != "" {
		conv.UIN = rec.UIN
	}
	if rec.EndpointID != "" {
		conv.EndpointID = rec.EndpointID
	}
	if rec.AppID != "" && conv.AppID != rec.AppID {
		conv.AppID = rec.AppID
		conv.AvatarURL = chatConsoleAvatarURL(conv.AppID, conv.TargetID)
	}
	if rec.FullMessage {
		conv.FullMessage = true
	}
	if rec.SenderName != "" && rec.Kind == chatConsoleKindC2C && !conv.NameManual &&
		conv.Name == chatConsoleDefaultName(conv.Kind, conv.TargetID) {
		conv.Name = rec.SenderName
	}
	conv.LastTS = msg.TS
	conv.LastText = chatConsolePreview(msg.Text)
	conv.UpdatedAt = now
	if msg.Dir == chatConsoleDirIn {
		conv.Unread++
	}
	s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Updates(map[string]interface{}{
		"uin":          conv.UIN,
		"endpoint_id":  conv.EndpointID,
		"app_id":       conv.AppID,
		"name":         conv.Name,
		"full_message": conv.FullMessage,
		"last_ts":      conv.LastTS,
		"last_text":    conv.LastText,
		"unread":       conv.Unread,
		"updated_at":   conv.UpdatedAt,
	})
	return conv
}

// pruneIfNeeded 按小时节流地清理过期消息。
func (s *ChatConsoleStore) pruneIfNeeded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
}

func (s *ChatConsoleStore) pruneLocked() {
	if s.db == nil || s.retentionDays <= 0 {
		return
	}
	now := time.Now()
	if !s.lastPrune.IsZero() && now.Sub(s.lastPrune) < chatConsolePruneInterval {
		return
	}
	s.lastPrune = now

	cutoff := now.AddDate(0, 0, -s.retentionDays).Unix()
	res := s.db.Where("ts < ?", cutoff).Delete(&chatConsoleMessageRow{})
	if res.Error == nil && res.RowsAffected > 0 {
		_ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
	}
	s.invalidateUsageLocked()
}

// ---------------- 公共 API：读取 ----------------

// Conversations 返回会话副本：置顶在最前，其余按最后消息时间倒序。
func (s *ChatConsoleStore) Conversations() []*ChatConsoleConversation {
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return []*ChatConsoleConversation{}
	}
	s.mu.Unlock()

	var rows []chatConsoleConversationRow
	if err := s.db.Where("last_ts > 0").Order("pinned DESC, last_ts DESC").Find(&rows).Error; err != nil {
		return []*ChatConsoleConversation{}
	}
	out := make([]*ChatConsoleConversation, 0, len(rows))
	for _, row := range rows {
		out = append(out, chatConsoleRowToConv(row))
	}
	return out
}

// Conversation 返回单个会话副本，不存在时返回 nil。
func (s *ChatConsoleStore) Conversation(key string) *ChatConsoleConversation {
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var row chatConsoleConversationRow
	if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
		return nil
	}
	return chatConsoleRowToConv(row)
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
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return nil, false, err
	}
	s.mu.Unlock()

	q := s.db.Where("conv_key = ?", key)
	if before > 0 {
		q = q.Where("ts < ?", before)
	}
	var rows []chatConsoleMessageRow
	if err := q.Order("ts DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	// 头像需要 AppID，会话只查一次
	appID := ""
	var convRow chatConsoleConversationRow
	if err := s.db.Select("app_id").Where("key = ?", key).Take(&convRow).Error; err == nil {
		appID = convRow.AppID
	}
	out := make([]ChatConsoleMessage, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		msg := chatConsoleRowToMessage(rows[i])
		msg.AvatarURL = chatConsoleAvatarURL(appID, msg.SenderOpenID)
		out = append(out, msg)
	}
	return out, hasMore, nil
}

// MarkRead 清空未读数。
func (s *ChatConsoleStore) MarkRead(key string) *ChatConsoleConversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return nil
	}
	var row chatConsoleConversationRow
	if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
		return nil
	}
	if row.Unread != 0 {
		s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Update("unread", 0)
		row.Unread = 0
	}
	conv := chatConsoleRowToConv(row)
	defer s.publish(ChatConsoleEvent{Type: "conversation", Conversation: conv})
	return conv
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
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return nil, err
	}
	var row chatConsoleConversationRow
	if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
		return nil, errors.New("会话不存在")
	}
	now := time.Now().Unix()
	s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Updates(map[string]interface{}{
		"name":        name,
		"name_manual": true,
		"updated_at":  now,
	})
	conv := chatConsoleRowToConv(row)
	conv.Name = name
	conv.NameManual = true
	conv.UpdatedAt = now
	defer s.publish(ChatConsoleEvent{Type: "conversation", Conversation: conv})
	return conv, nil
}

// SetPinned 置顶 / 取消置顶会话。置顶的会话在列表里始终排在前面。
func (s *ChatConsoleStore) SetPinned(key string, pinned bool) (*ChatConsoleConversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return nil, err
	}
	var row chatConsoleConversationRow
	if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
		return nil, errors.New("会话不存在")
	}
	s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Update("pinned", pinned)
	conv := chatConsoleRowToConv(row)
	conv.Pinned = pinned
	defer s.publish(ChatConsoleEvent{Type: "conversation", Conversation: conv})
	return conv, nil
}

// SetGroupInfo 写入官方返回的群名 / 成员数。手动改过名的会话不会被覆盖。
func (s *ChatConsoleStore) SetGroupInfo(key, name string, memberCount int) *ChatConsoleConversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return nil
	}
	var row chatConsoleConversationRow
	if err := s.db.Where("key = ?", key).Take(&row).Error; err != nil {
		return nil
	}
	conv := chatConsoleRowToConv(row)
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
		return nil
	}
	s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Updates(map[string]interface{}{
		"name":         conv.Name,
		"member_count": conv.MemberCount,
	})
	defer s.publish(ChatConsoleEvent{Type: "conversation", Conversation: conv})
	return conv
}

// Find 按 ID 查询一条消息（不存在时返回 nil）。
func (s *ChatConsoleStore) Find(key, msgID string) *ChatConsoleMessage {
	if msgID == "" {
		return nil
	}
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var row chatConsoleMessageRow
	err := s.db.Where("conv_key = ? AND (id = ? OR msg_id = ?)", key, msgID, msgID).
		Order("ts DESC").Take(&row).Error
	if err != nil {
		return nil
	}
	msg := chatConsoleRowToMessage(row)
	return &msg
}

// MarkRecalled 把一条已发送的消息标记为"已撤回"并推送事件。
func (s *ChatConsoleStore) MarkRecalled(key, msgID string) (*ChatConsoleMessage, error) {
	if msgID == "" {
		return nil, errors.New("消息 ID 为空")
	}
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	var row chatConsoleMessageRow
	err := s.db.Where("conv_key = ? AND (id = ? OR msg_id = ?)", key, msgID, msgID).
		Order("ts DESC").Take(&row).Error
	if err != nil {
		s.mu.Unlock()
		return nil, errors.New("没有找到这条消息（可能已被清理）")
	}
	now := time.Now().Unix()
	s.db.Model(&chatConsoleMessageRow{}).Where("id = ?", row.ID).Updates(map[string]interface{}{
		"recalled":    true,
		"recalled_at": now,
	})
	row.Recalled = true
	row.RecalledAt = now

	var convRow chatConsoleConversationRow
	var convCopy *ChatConsoleConversation
	if err := s.db.Where("key = ?", key).Take(&convRow).Error; err == nil && convRow.LastTS == row.TS {
		s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Update("last_text", "（消息已撤回）")
		convRow.LastText = "（消息已撤回）"
		c := chatConsoleRowToConv(convRow)
		convCopy = c
	}
	s.invalidateUsageLocked()
	s.mu.Unlock()

	msg := chatConsoleRowToMessage(row)
	s.publish(ChatConsoleEvent{Type: "recall", Conversation: convCopy, Message: &msg})
	return &msg, nil
}

// ---------------- 公共 API：保留设置 / 占用统计 / 清空 ----------------

// RetentionDays 返回当前保留天数（0 = 永久保留）。
func (s *ChatConsoleStore) RetentionDays() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return chatConsoleDefaultRetentionDays
	}
	return s.retentionDays
}

// SetRetentionDays 调整保留天数并立刻按新规则清理一次。
// 0 表示永久保留；上限 chatConsoleMaxRetentionDays。
func (s *ChatConsoleStore) SetRetentionDays(days int) int {
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return 0
	}
	s.retentionDays = clampChatConsoleRetention(days)
	s.saveSettingsLocked()
	s.lastPrune = time.Time{} // 绕过 1 小时节流，立刻清一次
	s.pruneLocked()
	retention := s.retentionDays
	s.mu.Unlock()

	s.publish(ChatConsoleEvent{Type: "settings"})
	return retention
}

type chatConsoleUsageCache struct {
	at    time.Time
	total int64
	items []ChatConsoleUsage
}

func (s *ChatConsoleStore) invalidateUsageLocked() {
	s.usageCache = nil
}

// Usage 统计整个目录（数据库 + 设置 + 旧版遗留）的占用，以及每个会话的估算占用。
// 结果缓存 15 秒。
func (s *ChatConsoleStore) Usage() (int64, []ChatConsoleUsage) {
	s.mu.Lock()
	if err := s.openLocked(); err != nil {
		s.mu.Unlock()
		return 0, nil
	}
	if c := s.usageCache; c != nil && time.Since(c.at) < chatConsoleUsageCacheTTL {
		total, items := c.total, c.items
		s.mu.Unlock()
		return total, items
	}
	convs := make([]*ChatConsoleConversation, 0)
	var rows []chatConsoleConversationRow
	if err := s.db.Find(&rows).Error; err == nil {
		for _, row := range rows {
			convs = append(convs, chatConsoleRowToConv(row))
		}
	}
	dir := s.dir
	s.mu.Unlock()

	total, items := s.scanUsage(dir, convs)

	s.mu.Lock()
	s.usageCache = &chatConsoleUsageCache{at: time.Now(), total: total, items: items}
	s.mu.Unlock()
	return total, items
}

// scanUsage 聚合每个会话的条数 / 时间范围 / 估算字节数，并统计目录里的实际文件。
func (s *ChatConsoleStore) scanUsage(dir string, convs []*ChatConsoleConversation) (int64, []ChatConsoleUsage) {
	items := make([]ChatConsoleUsage, 0, len(convs))
	byKey := make(map[string]*ChatConsoleUsage, len(convs))
	for _, conv := range convs {
		item := ChatConsoleUsage{Key: conv.Key, Name: conv.Name, Kind: conv.Kind}
		items = append(items, item)
		byKey[conv.Key] = &items[len(items)-1]
	}

	type aggRow struct {
		ConvKey  string
		Count    int
		Bytes    int64
		OldestTS int64
		NewestTS int64
	}
	var aggs []aggRow
	query := `SELECT conv_key,
		COUNT(*) AS count,
		SUM(LENGTH(COALESCE(text,'')) + LENGTH(COALESCE(media,'')) + LENGTH(COALESCE(quote,''))
		    + LENGTH(COALESCE(sender_id,'')) + LENGTH(COALESCE(sender_name,'')) + ?) AS bytes,
		MIN(ts) AS oldest_ts, MAX(ts) AS newest_ts
		FROM chat_console_messages GROUP BY conv_key`
	_ = s.db.Raw(query, chatConsoleRowOverheadBytes).Scan(&aggs).Error
	for _, agg := range aggs {
		item, ok := byKey[agg.ConvKey]
		if !ok {
			items = append(items, ChatConsoleUsage{Key: agg.ConvKey})
			item = &items[len(items)-1]
			byKey[agg.ConvKey] = item
		}
		item.Messages = agg.Count
		item.Bytes = agg.Bytes
		item.OldestTS = agg.OldestTS
		item.NewestTS = agg.NewestTS
		item.Days = chatConsoleDayCount(agg.OldestTS, agg.NewestTS)
	}

	var total int64
	for _, name := range []string{chatConsoleDBName, chatConsoleDBName + "-wal", chatConsoleDBName + "-shm", chatConsoleSettingsName} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil {
			total += info.Size()
		}
	}
	// 旧版遗留目录（导入后保留的一份备份）也算进总占用，方便骰主决定删不删
	if legacySize := chatConsoleDirSize(s.legacyDir()); legacySize > 0 {
		total += legacySize
		items = append(items, ChatConsoleUsage{
			Key:   chatConsoleLegacyDir,
			Name:  "旧版 jsonl 备份（可删除）",
			Bytes: legacySize,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Bytes > items[j].Bytes })
	return total, items
}

func chatConsoleDayCount(oldest, newest int64) int {
	if oldest <= 0 || newest <= 0 {
		return 0
	}
	days := int((newest-oldest)/(24*3600)) + 1
	if days < 1 {
		days = 1
	}
	return days
}

func chatConsoleDirSize(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			total += chatConsoleDirSize(filepath.Join(dir, e.Name()))
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// ClearConversation 清空单个会话的历史（保留会话元信息，群名/权限不丢）。
func (s *ChatConsoleStore) ClearConversation(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return err
	}
	if err := s.db.Where("conv_key = ?", key).Delete(&chatConsoleMessageRow{}).Error; err != nil {
		return err
	}
	now := time.Now().Unix()
	s.db.Model(&chatConsoleConversationRow{}).Where("key = ?", key).Updates(map[string]interface{}{
		"last_text":  "（已清空）",
		"unread":     0,
		"updated_at": now,
	})
	s.invalidateUsageLocked()
	_ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error

	var row chatConsoleConversationRow
	var convCopy *ChatConsoleConversation
	if err := s.db.Where("key = ?", key).Take(&row).Error; err == nil {
		c := chatConsoleRowToConv(row)
		convCopy = c
	}
	defer func() {
		if convCopy != nil {
			s.publish(ChatConsoleEvent{Type: "conversation", Conversation: convCopy})
		}
	}()
	return nil
}

// ClearAll 清空全部历史（保留 settings.json 里的保留天数），并尝试把数据库文件缩小。
func (s *ChatConsoleStore) ClearAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.openLocked(); err != nil {
		return err
	}
	if err := s.db.Where("1 = 1").Delete(&chatConsoleMessageRow{}).Error; err != nil {
		return err
	}
	if err := s.db.Where("1 = 1").Delete(&chatConsoleConversationRow{}).Error; err != nil {
		return err
	}
	// 旧版遗留备份也一起清掉，否则"清空全部"之后占用还是降不下来
	_ = os.RemoveAll(s.legacyDir())
	_ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
	_ = s.db.Exec("VACUUM").Error
	s.invalidateUsageLocked()
	return nil
}

// Flush 把 WAL 落盘（测试与关停时用）。SQLite 模式下写入本身就是持久的，
// 这里只是把 WAL 合并回主库文件。
func (s *ChatConsoleStore) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return
	}
	_ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)").Error
}

// ---------------- 内部：老版本 jsonl 迁移 ----------------

// migrateLegacyJSONLLocked 把 v1.1 及以前的 index.json + 按天 jsonl 导入 SQLite，
// 然后把原始文件整体挪到 legacy-jsonl/ 保留一份（不直接删，出问题还能查）。
func (s *ChatConsoleStore) migrateLegacyJSONLLocked() {
	indexPath := filepath.Join(s.dir, "index.json")
	rawIndex, indexErr := os.ReadFile(indexPath) //nolint:gosec // 内部路径
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}

	legacyConvs := map[string]*ChatConsoleConversation{}
	if indexErr == nil {
		var idx struct {
			Conversations map[string]*ChatConsoleConversation `json:"conversations"`
		}
		if err := json.Unmarshal(rawIndex, &idx); err == nil {
			for k, v := range idx.Conversations {
				if v == nil {
					continue
				}
				v.Key = k
				legacyConvs[chatConsoleSafeName(k)] = v
			}
		}
	}

	var convDirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, chatConsoleKindGroup+"_") || strings.HasPrefix(name, chatConsoleKindC2C+"_") ||
			strings.HasPrefix(name, "group-") || strings.HasPrefix(name, "c2c-") {
			convDirs = append(convDirs, name)
		}
	}
	if len(convDirs) == 0 && indexErr != nil {
		return
	}

	// 先导入会话元信息
	for _, conv := range legacyConvs {
		convRow := chatConsoleConvToRow(conv)
		if err := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&convRow).Error; err != nil {
			return
		}
	}

	// 再导入消息
	for _, dirName := range convDirs {
		var key string
		if conv := legacyConvs[dirName]; conv != nil {
			key = conv.Key
		} else {
			// 没有 index.json 时按目录名反推（group_xxx -> group:xxx）
			key = strings.Replace(dirName, "_", ":", 1)
		}
		convDir := filepath.Join(s.dir, dirName)
		files, err := os.ReadDir(convDir)
		if err != nil {
			continue
		}
		var batch []chatConsoleMessageRow
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(convDir, f.Name())) //nolint:gosec // 内部路径
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				var msg ChatConsoleMessage
				if err := json.Unmarshal([]byte(line), &msg); err != nil {
					continue
				}
				if msg.ID == "" {
					continue
				}
				batch = append(batch, chatConsoleMessageRow{
					ID:           msg.ID,
					MsgID:        msg.MsgID,
					ConvKey:      key,
					TS:           msg.TS,
					Dir:          msg.Dir,
					Source:       msg.Source,
					SenderID:     msg.SenderID,
					SenderName:   msg.SenderName,
					SenderOpenID: msg.SenderOpenID,
					Text:         msg.Text,
					Media:        chatConsoleMediaJSON(msg.Media),
					Quote:        chatConsoleQuoteJSON(msg.Quote),
					Recalled:     msg.Recalled,
					RecalledAt:   msg.RecalledAt,
				})
				if len(batch) >= 500 {
					s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(batch, 500)
					batch = batch[:0]
				}
			}
		}
		if len(batch) > 0 {
			s.db.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(batch, 500)
		}
	}

	// 迁移完成：把老文件挪进 legacy-jsonl/
	legacy := s.legacyDir()
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		return
	}
	if indexErr == nil {
		_ = os.Rename(indexPath, filepath.Join(legacy, "index.json"))
	}
	for _, dirName := range convDirs {
		_ = os.Rename(filepath.Join(s.dir, dirName), filepath.Join(legacy, dirName))
	}
}

// chatConsoleSafeName 把会话 key 映射成可用的目录 / 文件名（迁移时用）。
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
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
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
