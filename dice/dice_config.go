package dice

import (
	"time"

	"github.com/robfig/cron/v3"
	"golang.org/x/time/rate"
	"gopkg.in/yaml.v3"

	"sealdice-core/dice/censor"
	"sealdice-core/utils"
)

// ConfigVersion 当前设置版本
const (
	ConfigVersion     = 1
	ConfigVersionCode = 10300 // 旧的设置版本标记
)

type Config struct {
	d             *Dice `yaml:"-"`
	ConfigVersion int   `json:"configVersion" yaml:"configVersion"` // 配置版本

	// 基础设置
	BaseConfig `yaml:",inline"`
	// 刷屏警告设置
	RateLimitConfig `yaml:",inline"`
	// 退出不活跃设置
	QuitInactiveConfig `yaml:",inline"`
	// 扩展设置
	ExtConfig `yaml:",inline"`
	// 黑名单设置
	BanConfig `yaml:",inline"`
	// js 设置
	JsConfig `yaml:",inline"`
	// 跑团日志设置
	StoryLogConfig `yaml:",inline"`
	// 邮件设置
	MailConfig `yaml:",inline"`
	// 新闻设置
	NewsConfig `yaml:",inline"`
	// 敏感词设置
	CensorConfig `yaml:",inline"`
	// 公骰设置
	PublicDiceConfig `yaml:",inline"`
	// 商店设置
	StoreConfig `yaml:",inline"`

	// 其它设置，包含由于被导出无法从 Dice 上迁移过来的配置项，为了在 DefaultConfig 上统一设置默认值增加此结构
	DirtyConfig `yaml:",inline"`
}

func NewConfig(d *Dice) Config {
	c := DefaultConfig
	c.d = d

	// set other default
	c.BanList = &BanListInfo{Parent: c.d}
	c.BanList.Init()
	return c
}

func (c *Config) LoadYamlConfig(data []byte) error {
	err := yaml.Unmarshal(data, &c)
	if err != nil {
		return err
	}
	c.migrateOld2Version1()
	c.FixIdentityBindConfig()
	c.FixOfficialQQConfig()
	return nil
}

// FixOfficialQQConfig 收敛官方 QQ 相关的配置到合理范围。
//
// 重点是请求超时：老配置文件里没有这个字段（0），如果直接拿去 SetTimeout(0)
// 会导致请求立即超时，所以必须补成默认值。
func (c *Config) FixOfficialQQConfig() {
	const (
		minTimeoutSec = 5   // 官方文档建议上传接口超时 ≥5 秒
		maxTimeoutSec = 600 // 上限 10 分钟，避免误填一个巨大的值把机器人卡死
	)
	if c.OfficialQQRequestTimeoutSec <= 0 {
		c.OfficialQQRequestTimeoutSec = DefaultConfig.OfficialQQRequestTimeoutSec
	}
	if c.OfficialQQRequestTimeoutSec < minTimeoutSec {
		c.OfficialQQRequestTimeoutSec = minTimeoutSec
	}
	if c.OfficialQQRequestTimeoutSec > maxTimeoutSec {
		c.OfficialQQRequestTimeoutSec = maxTimeoutSec
	}
}

// FixIdentityBindConfig 把身份绑定相关配置收敛到合理范围。
// 老配置文件里这些字段为 0（缺省），需要补成默认值。
func (c *Config) FixIdentityBindConfig() {
	if c.IdentityBindCooldownSec < 0 {
		c.IdentityBindCooldownSec = 0
	}
	if c.IdentityBindCooldownSec > identityBindMaxCooldownSec {
		c.IdentityBindCooldownSec = identityBindMaxCooldownSec
	}
	if c.LogMultiBotDedupWindowSec <= 0 {
		c.LogMultiBotDedupWindowSec = DefaultConfig.LogMultiBotDedupWindowSec
	}
	if c.LogMultiBotDedupWindowSec > logDedupWindowMaxSec {
		c.LogMultiBotDedupWindowSec = logDedupWindowMaxSec
	}
	// 验证码相关
	if c.IdentityBindCodeLength <= 0 {
		c.IdentityBindCodeLength = DefaultConfig.IdentityBindCodeLength
	}
	if c.IdentityBindCodeLength < identityBindCodeMinLen {
		c.IdentityBindCodeLength = identityBindCodeMinLen
	}
	if c.IdentityBindCodeLength > identityBindCodeMaxLen {
		c.IdentityBindCodeLength = identityBindCodeMaxLen
	}
	if c.IdentityBindCodeExpireSec <= 0 {
		c.IdentityBindCodeExpireSec = DefaultConfig.IdentityBindCodeExpireSec
	}
	if c.IdentityBindCodeExpireSec < 60 {
		c.IdentityBindCodeExpireSec = 60
	}
	if c.IdentityBindCodeExpireSec > 3600 {
		c.IdentityBindCodeExpireSec = 3600
	}
}

// migrateOld2Version1 旧格式设置项的迁移
func (c *Config) migrateOld2Version1() {
	if c.ConfigVersion != 0 {
		return
	}
	c.ConfigVersion = ConfigVersion

	c.CommandCompatibleMode = DefaultConfig.CommandCompatibleMode

	if c.MaxExecuteTime == 0 {
		c.MaxExecuteTime = DefaultConfig.MaxExecuteTime
	}

	if c.MaxCocCardGen == 0 {
		c.MaxCocCardGen = DefaultConfig.MaxCocCardGen
	}

	if c.PersonalReplenishRateStr == "" {
		c.PersonalReplenishRateStr = DefaultConfig.PersonalReplenishRateStr
		c.PersonalReplenishRate = DefaultConfig.PersonalReplenishRate
	} else {
		if parsed, errParse := utils.ParseRate(c.PersonalReplenishRateStr); errParse == nil {
			c.PersonalReplenishRate = parsed
		} else {
			c.d.Logger.Errorf("解析PersonalReplenishRate失败: %v", errParse)
			c.PersonalReplenishRateStr = DefaultConfig.PersonalReplenishRateStr
			c.PersonalReplenishRate = DefaultConfig.PersonalReplenishRate
		}
	}

	if c.PersonalBurst == 0 {
		c.PersonalBurst = DefaultConfig.PersonalBurst
	}

	if c.GroupReplenishRateStr == "" {
		c.GroupReplenishRateStr = DefaultConfig.GroupReplenishRateStr
		c.GroupReplenishRate = DefaultConfig.GroupReplenishRate
	} else {
		if parsed, errParse := utils.ParseRate(c.GroupReplenishRateStr); errParse == nil {
			c.GroupReplenishRate = parsed
		} else {
			c.d.Logger.Errorf("解析GroupReplenishRate失败: %v", errParse)
			c.GroupReplenishRateStr = DefaultConfig.GroupReplenishRateStr
			c.GroupReplenishRate = DefaultConfig.GroupReplenishRate
		}
	}

	if c.GroupBurst == 0 {
		c.GroupBurst = DefaultConfig.GroupBurst
	}

	if c.VersionCode != 0 && c.VersionCode < 10001 {
		c.AliveNoticeValue = DefaultConfig.AliveNoticeValue
	}

	if c.VersionCode != 0 && c.VersionCode < 10003 {
		c.LogSizeNoticeCount = DefaultConfig.LogSizeNoticeCount
		c.LogSizeNoticeEnable = DefaultConfig.LogSizeNoticeEnable
		c.CustomReplyConfigEnable = DefaultConfig.CustomReplyConfigEnable
	}

	if c.VersionCode != 0 && c.VersionCode < 10004 {
		c.AutoReloginEnable = DefaultConfig.AutoReloginEnable
	}
}

type BaseConfig struct {
	CommandCompatibleMode    bool       `json:"-"                       yaml:"commandCompatibleMode"`
	LastSavedTime            *time.Time `json:"-"                       yaml:"lastSavedTime"`
	NoticeIDs                []string   `json:"noticeIds"               yaml:"noticeIds"`               // 通知ID
	OnlyLogCommandInGroup    bool       `json:"onlyLogCommandInGroup"   yaml:"onlyLogCommandInGroup"`   // 日志中仅记录命令
	OnlyLogCommandInPrivate  bool       `json:"onlyLogCommandInPrivate" yaml:"onlyLogCommandInPrivate"` // 日志中仅记录命令
	VersionCode              int        `json:"versionCode"             yaml:"versionCode"`             // 版本ID(配置文件)
	MessageDelayRangeStart   float64    `json:"messageDelayRangeStart"  yaml:"messageDelayRangeStart"`  // 指令延迟区间
	MessageDelayRangeEnd     float64    `json:"messageDelayRangeEnd"    yaml:"messageDelayRangeEnd"`
	WorkInQQChannel          bool       `json:"workInQQChannel"         yaml:"workInQQChannel"`
	QQChannelAutoOn          bool       `json:"QQChannelAutoOn"         yaml:"QQChannelAutoOn"`           // QQ频道中自动开启(默认不开)
	QQChannelLogMessage      bool       `json:"QQChannelLogMessage"     yaml:"QQChannelLogMessage"`       // QQ频道中记录消息(默认不开)
	QQEnablePoke             bool       `json:"QQEnablePoke"            yaml:"QQEnablePoke"`              // 启用戳一戳
	OfficialQQFileSendBase64 bool       `json:"officialQQFileSendBase64" yaml:"officialQQFileSendBase64"` // 是否使用base64发送本地/非公网文件
	OfficialQQUseMarkdown    bool       `json:"officialQQUseMarkdown"    yaml:"officialQQUseMarkdown"`    // 是否自动把消息全转为markdown类型消息
	TextCmdTrustOnly         bool       `json:"textCmdTrustOnly"        yaml:"textCmdTrustOnly"`          // 只允许信任用户或master使用text指令
	IgnoreUnaddressedBotCmd  bool       `json:"ignoreUnaddressedBotCmd" yaml:"ignoreUnaddressedBotCmd"`   // 不响应群聊裸bot指令
	UILogLimit               int64      `json:"-"                       yaml:"UILogLimit"`
	FriendAddComment         string     `json:"friendAddComment"        yaml:"friendAddComment"` // 加好友验证信息
	CustomReplyConfigEnable  bool       `json:"customReplyConfigEnable" yaml:"customReplyConfigEnable"`
	AutoReloginEnable        bool       `json:"autoReloginEnable"       yaml:"autoReloginEnable"`    // 启用自动重新登录
	RefuseGroupInvite        bool       `json:"refuseGroupInvite"       yaml:"refuseGroupInvite"`    // 拒绝加入新群
	UpgradeWindowID          string     `json:"-"                       yaml:"upgradeWindowId"`      // 执行升级指令的窗口
	UpgradeEndpointID        string     `json:"-"                       yaml:"upgradeEndpointId"`    // 执行升级指令的端点
	BotExtFreeSwitch         bool       `json:"botExtFreeSwitch"        yaml:"botExtFreeSwitch"`     // 允许任意人员开关: 否则邀请者、群主、管理员、master有权限
	BotExitWithoutAt         bool       `json:"botExitWithoutAt"        yaml:"botExitWithoutAt"`     // 不@骰娘即可执行退群指令
	TrustOnlyMode            bool       `json:"trustOnlyMode"           yaml:"trustOnlyMode"`        // 只有信任的用户/master可以拉群和使用
	AliveNoticeEnable        bool       `json:"aliveNoticeEnable"       yaml:"aliveNoticeEnable"`    // 定时通知
	AliveNoticeValue         string     `json:"aliveNoticeValue"        yaml:"aliveNoticeValue"`     // 定时通知间隔
	ReplyDebugMode           bool       `json:"replyDebugMode"          yaml:"replyDebugMode"`       // 回复调试
	PlayerNameWrapEnable     bool       `json:"playerNameWrapEnable"    yaml:"playerNameWrapEnable"` // 启用玩家名称外框
	DiceRandomMode           string     `json:"diceRandomMode"          yaml:"diceRandomMode"`       // 骰点随机模式

	VMVersionForReply      string `json:"VMVersionForReply"      yaml:"VMVersionForReply"`      // 自定义回复使用的vm版本
	VMVersionForDeck       string `json:"VMVersionForDeck"       yaml:"VMVersionForDeck"`       // 牌堆使用的vm版本
	VMVersionForCustomText string `json:"VMVersionForCustomText" yaml:"VMVersionForCustomText"` // 自定义文案使用的vm版本
	VMVersionForMsg        string `json:"VMVersionForMsg"        yaml:"VMVersionForMsg"`        // 消息里使用的vm版本，也包括可能的一些边角类型的执行选择

	// TODO: 历史遗留问题，由于不输出DICE日志效果过差，已经抹除日志输出选项，剩余两个选项，私以为可以想办法也抹除掉。
	Name    string `yaml:"name"`    // 名称，默认为default
	DataDir string `yaml:"dataDir"` // 数据路径，为./data/{name}，例如data/default

	OfficialQQMigrationEnable bool `json:"officialQQEnableIdentityMigration" yaml:"officialQQEnableIdentityMigration"` // 启动 QQ 官方旧身份数据迁移

	// ==========================================================================
	// 以下字段全部是 fork 新增，**统一追加在 BaseConfig 末尾**。
	//
	// 这个约定是为了合并省事，**不是**编译要求：DefaultConfig 里各配置段
	// （BaseConfig{} / RateLimitConfig{} …）都是带字段名的 keyed 字面量，
	// 往中间插字段不会错位；真正没写字段名的只有最外层
	// Config{nil, ConfigVersion, …}，而它一旦增删会因为各段类型不同直接编译报错。
	// 追加在末尾的好处是：上游也在末尾追加字段时只会产生一处相邻冲突，手工解一下即可。
	// ==========================================================================

	// OfficialQQRequestTimeoutSec 官方 QQ OpenAPI 的单次请求超时（秒）。
	// 这个超时作用在 SDK 的 resty client 上，覆盖文本发送、富媒体上传、拉取机器人信息等全部请求。
	// 默认 60 秒：官方文档建议上传接口超时 ≥5 秒，而用 URL 上传时腾讯要先下载完整个文件才回响应头，
	// 3 秒会导致语音/文件经常报 "context deadline exceeded"。发大文件建议调到 120 以上。
	OfficialQQRequestTimeoutSec int64 `json:"officialQQRequestTimeoutSec" yaml:"officialQQRequestTimeoutSec"`
	// OfficialQQChunkedUploadEnable 是否对「本地文件发送」启用分片上传。
	// 默认关闭：先用旧路径保证稳定，测通后再打开。
	// 打开后本地文件走 upload_prepare → 分片 PUT → upload_part_finish → 合并，
	// 好处是**能保留文件名**（file_data/base64 方式腾讯不支持自定义文件名，会显示"未命名"）。
	OfficialQQChunkedUploadEnable bool `json:"officialQQChunkedUploadEnable" yaml:"officialQQChunkedUploadEnable"`

	// IdentityBindEnable 是否允许用户使用 .bind 系列指令，把 QQ 官方机器人的身份
	// 绑定回迁移前的旧账号（例如 NapCat 时代的 QQ:12345）。
	IdentityBindEnable bool `json:"identityBindEnable" yaml:"identityBindEnable"`
	// IdentityBindCooldownSec 同一个用户两次发起绑定之间的最小间隔（秒）。
	IdentityBindCooldownSec int64 `json:"identityBindCooldownSec" yaml:"identityBindCooldownSec"`

	// LogMultiBotDedupWindowSec 同一条玩家消息在多久内只记一次日志（秒）。
	//
	// 默认 5 秒，与上游原实现完全一致：按 msg.RawID 去重，只去重
	// 「同一个连接重复推送的同一条消息」，不会误伤任何真实发言。
	//
	// 调大（例如 30）会切换成按「群+人+正文」判重，TTL 也随之变长。
	//
	// ⚠️ 实测结论（第二轮排查）：**它并不能实现"跨 bot 去重"**。
	// 判定键里用的是各连接上报的**真实**ID —— 官方侧是 OpenQQ-Group:…/OpenQQ:…，
	// 民间侧是 QQ-Group:…/QQ:…，同一条消息经两条连接各收一次时两边算出的键不同，
	// 永远匹配不上。能命中的只有同一连接被重复推送的情况。
	// 而代价是真的：同一人在窗口内发的两条正文完全相同的消息会被并成一条。
	// 所以**目前不建议调大**；真要跨 bot 去重，得先把这个键改成归一后的身份。
	LogMultiBotDedupWindowSec int64 `json:"logMultiBotDedupWindowSec" yaml:"logMultiBotDedupWindowSec"`

	// IdentityBindUseVerificationCode 是否用「验证码」验证身份归属。**默认开启**。
	//
	// 这是防抢号的唯一手段：答题只能拦住不知道你信息的人，拦不住"知道你旧 QQ 号
	// 又猜得到你卡名"的人。验证码发给被声明的旧账号，只有真正持有它的人才能确认。
	//
	// 投递通道（**自动判断，不需要额外开关**）：
	//   · 民间 bot（OneBot）在线可发私聊 → 私聊通道可用
	//   · 「邮箱通知」配全了（发件邮箱 / 密钥 / SMTP）→ 邮箱通道可用（寄 QQ 邮箱）
	//   两条都可用时按 IdentityBindPreferEmailCode 决定先后。
	//
	// 关掉它 = 放弃防抢号，仅在骰主完全无法提供任何通道时才考虑。
	IdentityBindUseVerificationCode bool `json:"identityBindUseVerificationCode" yaml:"identityBindUseVerificationCode"`
	// IdentityBindCodeLength 验证码位数，4~8，默认 6。
	IdentityBindCodeLength int64 `json:"identityBindCodeLength" yaml:"identityBindCodeLength"`
	// IdentityBindCodeExpireSec 验证码有效期（秒），60~3600，默认 600（10 分钟）。
	IdentityBindCodeExpireSec int64 `json:"identityBindCodeExpireSec" yaml:"identityBindCodeExpireSec"`
	// IdentityBindPreferEmailCode 两条通道都可用时，是否优先用邮箱。
	//
	// 邮箱通道**没有独立开关**：只要「邮箱通知」配全了就自动可用，
	// 地址规则是 旧 QQ 号 → <QQ号>@qq.com（与海豹现有邮件通知逻辑一致）。
	// 这是唯一"零配置又有约束力"的方案：QQ 邮箱绑定 QQ 号，所以能证明账号归属。
	// **不会**采用用户自己填的邮箱——那既证明不了归属，又会让骰子变成发信机。
	//
	// 默认 false：先试私聊，发不出去才走邮箱。
	// 打开后顺序反过来：先寄邮箱，邮箱不可用再退回私聊。
	IdentityBindPreferEmailCode bool `json:"identityBindPreferEmailCode" yaml:"identityBindPreferEmailCode"`
}

type RateLimitConfig struct {
	RateLimitEnabled         bool       `json:"rateLimitEnabled"      yaml:"rateLimitEnabled"`      // 启用频率限制 (刷屏限制)
	PersonalReplenishRateStr string     `json:"personalReplenishRate" yaml:"personalReplenishRate"` // 个人刷屏警告速率，字符串格式
	PersonalReplenishRate    rate.Limit `json:"-"                     yaml:"-"`                     // 个人刷屏警告速率
	GroupReplenishRateStr    string     `json:"groupReplenishRate"    yaml:"groupReplenishRate"`    // 群组刷屏警告速率，字符串格式
	GroupReplenishRate       rate.Limit `json:"-"                     yaml:"-"`                     // 群组刷屏警告速率
	PersonalBurst            int64      `json:"personalBurst"         yaml:"personalBurst"`         // 个人自定义上限
	GroupBurst               int64      `json:"groupBurst"            yaml:"groupBurst"`            // 群组自定义上限
}

type QuitInactiveConfig struct {
	QuitInactiveThreshold time.Duration `json:"-" yaml:"quitInactiveThreshold"` // 退出不活跃群组的时间阈值
	quitInactiveCronEntry cron.EntryID

	QuitInactiveThresholdDays float64 `json:"quitInactiveThreshold" yaml:"-"` // 为了和前端通信

	QuitInactiveNoticeSummaryMode bool `json:"quitInactiveNoticeSummaryMode" yaml:"quitInactiveNoticeSummaryMode"` // 自动退群通知改为任务开始/结束摘要

	QuitInactiveBatchSize int64 `json:"quitInactiveBatchSize" yaml:"quitInactiveBatchSize"` // 退出不活跃群组的批量大小
	QuitInactiveBatchWait int64 `json:"quitInactiveBatchWait" yaml:"quitInactiveBatchWait"` // 退出不活跃群组的批量等待时间（分）
}

type ExtConfig struct {
	DefaultCocRuleIndex int64 `jsbind:"defaultCocRuleIndex" json:"-" yaml:"defaultCocRuleIndex"`                   // 默认coc index
	MaxExecuteTime      int64 `jsbind:"maxExecuteTime"      json:"-" yaml:"maxExecuteTime"`                        // 最大骰点次数
	MaxCocCardGen       int64 `jsbind:"maxCocCardGen"       json:"-" yaml:"maxCocCardGen"`                         // 最大coc制卡数
	CocCardMergeForward bool  `jsbind:"cocCardMergeForward" json:"cocCardMergeForward" yaml:"cocCardMergeForward"` // COC制卡是否使用合并转发（默认关闭）

	ExtDefaultSettings []*ExtDefaultSettingItem `json:"extDefaultSettings" yaml:"extDefaultSettings"` // 新群扩展按此顺序加载
}

type BanConfig struct {
	BanList *BanListInfo `json:"-" yaml:"banList"`
}

type JsConfig struct {
	JsEnable          bool            `json:"jsEnable"          yaml:"jsEnable"`
	DisabledJsScripts map[string]bool `json:"disabledJsScripts" yaml:"disabledJsScripts"` // 作为set
}

type StoryLogConfig struct {
	LogSizeNoticeEnable bool `json:"logSizeNoticeEnable" yaml:"logSizeNoticeEnable"` // 开启日志数量提示
	LogSizeNoticeCount  int  `json:"logSizeNoticeCount"  yaml:"LogSizeNoticeCount"`  // 日志数量提示阈值，默认500
}

type MailConfig struct {
	MailEnable   bool   `json:"mailEnable"   yaml:"mailEnable"`   // 是否启用
	MailFrom     string `json:"mailFrom"     yaml:"mailFrom"`     // 邮箱来源
	MailPassword string `json:"mailPassword" yaml:"mailPassword"` // 邮箱密钥/密码
	MailSMTP     string `json:"mailSmtp"     yaml:"mailSmtp"`     // 邮箱 smtp 地址
}

type NewsConfig struct {
	NewsMark string `json:"newsMark" yaml:"newsMark"` // 已读新闻的md5
}

type PublicDiceConfig struct {
	Enable bool   `json:"publicDiceEnable" yaml:"publicDiceEnable"`
	ID     string `json:"publicDiceId"     yaml:"publicDiceId"`
	Name   string `json:"publicDiceName"   yaml:"publicDiceName"`
	Brief  string `json:"publicDiceBrief"  yaml:"publicDiceBrief"`
	Note   string `json:"publicDiceNote"   yaml:"publicDiceNote"`
	Avatar string `json:"publicDiceAvatar" yaml:"publicDiceAvatar"`
}

type CensorConfig struct {
	EnableCensor         bool                   `json:"enableCensor"         yaml:"enableCensor"` // 启用敏感词审查
	CensorMode           CensorMode             `json:"censorMode"           yaml:"censorMode"`
	CensorThresholds     map[censor.Level]int   `json:"censorThresholds"     yaml:"censorThresholds"` // 敏感词阈值
	CensorHandlers       map[censor.Level]uint8 `json:"censorHandlers"       yaml:"censorHandlers"`
	CensorScores         map[censor.Level]int   `json:"censorScores"         yaml:"censorScores"`         // 敏感词怒气值
	CensorCaseSensitive  bool                   `json:"censorCaseSensitive"  yaml:"censorCaseSensitive"`  // 敏感词大小写敏感
	CensorMatchPinyin    bool                   `json:"censorMatchPinyin"    yaml:"censorMatchPinyin"`    // 敏感词匹配拼音
	CensorFilterRegexStr string                 `json:"censorFilterRegexStr" yaml:"censorFilterRegexStr"` // 敏感词过滤字符正则
}

type DirtyConfig struct {
	DeckList      []*DeckInfo `yaml:"-"` // 牌堆信息
	CommandPrefix []string    `yaml:"-"` // 指令前导
	DiceMasters   []string    `yaml:"-"` // 骰主设置，需要格式: 平台:帐号
}

type StoreConfig struct {
	BackendUrls         []string `json:"backendUrls" yaml:"backendUrls"`
	DisabledBackendUrls []string `json:"disabledBackendUrls" yaml:"disabledBackendUrls"`
}
