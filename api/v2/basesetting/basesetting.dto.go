package basesetting

import (
	"github.com/danielgtaylor/huma/v2"
)

type BaseSettingExtDefaultSettingItem struct {
	Name            string          `json:"name"`
	AutoActive      bool            `json:"autoActive"`
	DisabledCommand map[string]bool `json:"disabledCommand"`
	Loaded          bool            `json:"loaded"`
}

type BaseSettingValueResp struct {
	CommandPrefix            []string `json:"commandPrefix"`
	DiceMasters              []string `json:"diceMasters"`
	NoticeIds                []string `json:"noticeIds"`
	MasterUnlockCode         string   `json:"masterUnlockCode"`
	UIPassword               string   `json:"uiPassword"`
	MailEnable               bool     `json:"mailEnable"`
	MailFrom                 string   `json:"mailFrom"`
	MailPassword             string   `json:"mailPassword"`
	MailSmtp                 string   `json:"mailSmtp"`
	TrustOnlyMode            bool     `json:"trustOnlyMode"`
	BotExtFreeSwitch         bool     `json:"botExtFreeSwitch"`
	QQEnablePoke             bool     `json:"QQEnablePoke"`
	OfficialQQFileSendBase64 bool     `json:"officialQQFileSendBase64"`
	OfficialQQUseMarkdown    bool     `json:"officialQQUseMarkdown"`
	// fork 新增：官方 QQ 请求超时与分片上传
	OfficialQQRequestTimeoutSec   int64 `json:"officialQQRequestTimeoutSec"`
	OfficialQQChunkedUploadEnable bool  `json:"officialQQChunkedUploadEnable"`
	// fork 新增：身份与日志绑定（QQ 官方 ↔ 旧 QQ 号/旧群）
	IdentityBindEnable              bool  `json:"identityBindEnable"`
	IdentityBindUseVerificationCode bool  `json:"identityBindUseVerificationCode"`
	IdentityBindPreferEmailCode     bool  `json:"identityBindPreferEmailCode"`
	IdentityBindCodeLength          int64 `json:"identityBindCodeLength"`
	IdentityBindCodeExpireSec       int64 `json:"identityBindCodeExpireSec"`
	IdentityBindCooldownSec         int64 `json:"identityBindCooldownSec"`
	// fork 新增：同一条消息的日志去重窗口
	LogMultiBotDedupWindowSec int64                               `json:"logMultiBotDedupWindowSec"`
	TextCmdTrustOnly          bool                                `json:"textCmdTrustOnly"`
	IgnoreUnaddressedBotCmd   bool                                `json:"ignoreUnaddressedBotCmd"`
	AliveNoticeEnable         bool                                `json:"aliveNoticeEnable"`
	AliveNoticeValue          string                              `json:"aliveNoticeValue"`
	LogSizeNoticeEnable       bool                                `json:"logSizeNoticeEnable"`
	LogSizeNoticeCount        int                                 `json:"logSizeNoticeCount"`
	PlayerNameWrapEnable      bool                                `json:"playerNameWrapEnable"`
	DiceRandomMode            string                              `json:"diceRandomMode"`
	OnlyLogCommandInGroup     bool                                `json:"onlyLogCommandInGroup"`
	OnlyLogCommandInPrivate   bool                                `json:"onlyLogCommandInPrivate"`
	RateLimitEnabled          bool                                `json:"rateLimitEnabled"`
	PersonalReplenishRate     string                              `json:"personalReplenishRate"`
	PersonalBurst             int64                               `json:"personalBurst"`
	GroupReplenishRate        string                              `json:"groupReplenishRate"`
	GroupBurst                int64                               `json:"groupBurst"`
	ServeAddress              string                              `json:"serveAddress"`
	TrayTooltip               string                              `json:"trayTooltip"`
	RefuseGroupInvite         bool                                `json:"refuseGroupInvite"`
	FriendAddComment          string                              `json:"friendAddComment"`
	WorkInQQChannel           bool                                `json:"workInQQChannel"`
	QQChannelAutoOn           bool                                `json:"QQChannelAutoOn"`
	QQChannelLogMessage       bool                                `json:"QQChannelLogMessage"`
	DefaultCocRuleIndex       string                              `json:"defaultCocRuleIndex"`
	MaxCocCardGen             string                              `json:"maxCocCardGen"`
	MaxExecuteTime            string                              `json:"maxExecuteTime"`
	MessageDelayRangeStart    float64                             `json:"messageDelayRangeStart"`
	MessageDelayRangeEnd      float64                             `json:"messageDelayRangeEnd"`
	QuitInactiveThreshold     float64                             `json:"quitInactiveThreshold"`
	QuitInactiveBatchSize     int64                               `json:"quitInactiveBatchSize"`
	QuitInactiveBatchWait     int64                               `json:"quitInactiveBatchWait"`
	ExtDefaultSettings        []*BaseSettingExtDefaultSettingItem `json:"extDefaultSettings"`
}

// BaseSettingPatch distinguishes omitted fields from explicit false, zero and empty lists.
type BaseSettingPatch struct {
	CommandPrefix            *[]string `json:"commandPrefix,omitempty"`
	DiceMasters              *[]string `json:"diceMasters,omitempty"`
	NoticeIds                *[]string `json:"noticeIds,omitempty"`
	UIPassword               *string   `json:"uiPassword,omitempty"`
	MailEnable               *bool     `json:"mailEnable,omitempty"`
	MailFrom                 *string   `json:"mailFrom,omitempty"`
	MailPassword             *string   `json:"mailPassword,omitempty"`
	MailSmtp                 *string   `json:"mailSmtp,omitempty"`
	TrustOnlyMode            *bool     `json:"trustOnlyMode,omitempty"`
	BotExtFreeSwitch         *bool     `json:"botExtFreeSwitch,omitempty"`
	QQEnablePoke             *bool     `json:"QQEnablePoke,omitempty"`
	OfficialQQFileSendBase64 *bool     `json:"officialQQFileSendBase64,omitempty"`
	OfficialQQUseMarkdown    *bool     `json:"officialQQUseMarkdown,omitempty"`
	// fork 新增（同上）
	OfficialQQRequestTimeoutSec     *int64                               `json:"officialQQRequestTimeoutSec,omitempty"`
	OfficialQQChunkedUploadEnable   *bool                                `json:"officialQQChunkedUploadEnable,omitempty"`
	IdentityBindEnable              *bool                                `json:"identityBindEnable,omitempty"`
	IdentityBindUseVerificationCode *bool                                `json:"identityBindUseVerificationCode,omitempty"`
	IdentityBindPreferEmailCode     *bool                                `json:"identityBindPreferEmailCode,omitempty"`
	IdentityBindCodeLength          *int64                               `json:"identityBindCodeLength,omitempty"`
	IdentityBindCodeExpireSec       *int64                               `json:"identityBindCodeExpireSec,omitempty"`
	IdentityBindCooldownSec         *int64                               `json:"identityBindCooldownSec,omitempty"`
	LogMultiBotDedupWindowSec       *int64                               `json:"logMultiBotDedupWindowSec,omitempty"`
	TextCmdTrustOnly                *bool                                `json:"textCmdTrustOnly,omitempty"`
	IgnoreUnaddressedBotCmd         *bool                                `json:"ignoreUnaddressedBotCmd,omitempty"`
	AliveNoticeEnable               *bool                                `json:"aliveNoticeEnable,omitempty"`
	AliveNoticeValue                *string                              `json:"aliveNoticeValue,omitempty"`
	LogSizeNoticeEnable             *bool                                `json:"logSizeNoticeEnable,omitempty"`
	LogSizeNoticeCount              *int                                 `json:"logSizeNoticeCount,omitempty"`
	PlayerNameWrapEnable            *bool                                `json:"playerNameWrapEnable,omitempty"`
	DiceRandomMode                  *string                              `json:"diceRandomMode,omitempty"`
	OnlyLogCommandInGroup           *bool                                `json:"onlyLogCommandInGroup,omitempty"`
	OnlyLogCommandInPrivate         *bool                                `json:"onlyLogCommandInPrivate,omitempty"`
	RateLimitEnabled                *bool                                `json:"rateLimitEnabled,omitempty"`
	PersonalReplenishRate           *string                              `json:"personalReplenishRate,omitempty"`
	PersonalBurst                   *int64                               `json:"personalBurst,omitempty"`
	GroupReplenishRate              *string                              `json:"groupReplenishRate,omitempty"`
	GroupBurst                      *int64                               `json:"groupBurst,omitempty"`
	ServeAddress                    *string                              `json:"serveAddress,omitempty"`
	TrayTooltip                     *string                              `json:"trayTooltip,omitempty"`
	RefuseGroupInvite               *bool                                `json:"refuseGroupInvite,omitempty"`
	FriendAddComment                *string                              `json:"friendAddComment,omitempty"`
	WorkInQQChannel                 *bool                                `json:"workInQQChannel,omitempty"`
	QQChannelAutoOn                 *bool                                `json:"QQChannelAutoOn,omitempty"`
	QQChannelLogMessage             *bool                                `json:"QQChannelLogMessage,omitempty"`
	DefaultCocRuleIndex             *string                              `json:"defaultCocRuleIndex,omitempty"`
	MaxCocCardGen                   *string                              `json:"maxCocCardGen,omitempty"`
	MaxExecuteTime                  *string                              `json:"maxExecuteTime,omitempty"`
	MessageDelayRangeStart          *float64                             `json:"messageDelayRangeStart,omitempty"`
	MessageDelayRangeEnd            *float64                             `json:"messageDelayRangeEnd,omitempty"`
	QuitInactiveThreshold           *float64                             `json:"quitInactiveThreshold,omitempty"`
	QuitInactiveBatchSize           *int64                               `json:"quitInactiveBatchSize,omitempty"`
	QuitInactiveBatchWait           *int64                               `json:"quitInactiveBatchWait,omitempty"`
	ExtDefaultSettings              *[]*BaseSettingExtDefaultSettingItem `json:"extDefaultSettings,omitempty"`
}

type BaseSettingUpdateReq struct {
	Body BaseSettingPatch `json:"body"`
}

type BaseSettingActionResp struct {
	Success bool   `json:"success"`
	Err     string `json:"err,omitempty"`
}

type BaseSettingUpgradeForm struct {
	File huma.FormFile `form:"file" required:"true"`
}

type BaseSettingUpgradeReq struct {
	RawBody huma.MultipartFormFiles[BaseSettingUpgradeForm]
}
