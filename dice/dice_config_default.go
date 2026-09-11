package dice

import (
	"time"

	"golang.org/x/time/rate"

	"sealdice-core/dice/censor"
)

var DefaultConfig = Config{
	nil,
	ConfigVersion,
	BaseConfig{
		CommandCompatibleMode:    true, // 一直为true即可
		LastSavedTime:            nil,
		NoticeIDs:                []string{"UI:1001"},
		OnlyLogCommandInGroup:    false,
		OnlyLogCommandInPrivate:  false,
		VersionCode:              ConfigVersionCode,
		MessageDelayRangeStart:   0.0,
		MessageDelayRangeEnd:     0.4,
		WorkInQQChannel:          true,
		QQChannelAutoOn:          false,
		QQChannelLogMessage:      false,
		QQEnablePoke:             true,
		OfficialQQFileSendBase64: false,
		OfficialQQUseMarkdown:    false,
		// 3 秒会让语音/文件上传经常超时（腾讯要先下载完 URL 才回响应头），默认放宽到 60 秒
		OfficialQQRequestTimeoutSec: 60,
		// 分片上传默认关闭：先用旧的 file_data 路径保证稳定，测通后再打开。
		// 打开后本地文件走分片上传，可保留文件名；关着时文件名会显示"未命名"。
		OfficialQQChunkedUploadEnable: false,
		TextCmdTrustOnly:              true,
		IgnoreUnaddressedBotCmd:       false,
		UILogLimit:                    0,
		FriendAddComment:              "",
		CustomReplyConfigEnable:       false,
		AutoReloginEnable:             false,
		RefuseGroupInvite:             false,
		UpgradeWindowID:               "",
		UpgradeEndpointID:             "",
		BotExtFreeSwitch:              false,
		BotExitWithoutAt:              false,
		TrustOnlyMode:                 false,
		AliveNoticeEnable:             false,
		AliveNoticeValue:              "@every 3h",
		ReplyDebugMode:                false,
		PlayerNameWrapEnable:          true,
		DiceRandomMode:                string(DiceRandomModePCG),
		VMVersionForReply:             "v1",
		VMVersionForDeck:              "v2",
		VMVersionForCustomText:        "v2",
		VMVersionForMsg:               "v2",
		Name:                          "default",
		DataDir:                       "data/default",

		OfficialQQMigrationEnable: false,

		IdentityBindEnable:          false,
		IdentityBindQuestionCount:   1,
		IdentityBindCooldownSec:     60,
		IdentityBindFailCooldownSec: 12 * 3600, // 答错锁 12 小时

		// 同一条玩家消息在多久内只记一次日志。默认 5 秒 = 上游原行为，
		// 只挡「同一个连接重复推送」，不会合并真实发言。
		// 只有同群同时挂官方 bot 和民间 bot 时，才需要把它调大（例如 30），
		// 此时才会启用「按 群+人+正文 跨连接去重」——代价是同一人窗口内
		// 发的两条完全相同的消息会被并成一条，所以默认不开。
		LogMultiBotDedupWindowSec: 5,
	},
	RateLimitConfig{
		RateLimitEnabled:         false,
		PersonalReplenishRateStr: "@every 3s",
		PersonalReplenishRate:    rate.Every(time.Second * 3),
		GroupReplenishRateStr:    "@every 3s",
		GroupReplenishRate:       rate.Every(time.Second * 3),
		PersonalBurst:            3,
		GroupBurst:               3,
	},
	QuitInactiveConfig{
		QuitInactiveThreshold:         0,
		quitInactiveCronEntry:         0,
		QuitInactiveNoticeSummaryMode: false,
		QuitInactiveBatchSize:         10,
		QuitInactiveBatchWait:         30,
	},
	ExtConfig{
		DefaultCocRuleIndex: 0,
		MaxExecuteTime:      12,
		MaxCocCardGen:       5,
		CocCardMergeForward: false,
		ExtDefaultSettings:  make([]*ExtDefaultSettingItem, 0),
	},
	BanConfig{
		BanList: nil,
	},
	JsConfig{
		JsEnable:          true,
		DisabledJsScripts: make(map[string]bool),
	},
	StoryLogConfig{
		LogSizeNoticeEnable: true,
		LogSizeNoticeCount:  500,
	},
	MailConfig{
		MailEnable:   false,
		MailFrom:     "",
		MailPassword: "",
		MailSMTP:     "",
	},
	NewsConfig{
		NewsMark: "",
	},
	CensorConfig{
		EnableCensor:         false,
		CensorMode:           0,
		CensorThresholds:     make(map[censor.Level]int),
		CensorHandlers:       make(map[censor.Level]uint8),
		CensorScores:         make(map[censor.Level]int),
		CensorCaseSensitive:  false,
		CensorMatchPinyin:    false,
		CensorFilterRegexStr: "",
	},
	PublicDiceConfig{
		Enable: false,
	},
	StoreConfig{
		BackendUrls:         []string{},
		DisabledBackendUrls: []string{},
	},
	DirtyConfig{
		DeckList: nil,
		CommandPrefix: []string{
			"!",
			".",
			"。",
			"/",
		},
		DiceMasters: []string{"UI:1001"},
	},
}
