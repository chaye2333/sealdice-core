package dice

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"sealdice-core/dice/censor"
	"sealdice-core/dice/service"
	"sealdice-core/utils/dboperator/engine"
)

type CensorMode int

const (
	OnlyOutputReply CensorMode = iota
	OnlyInputCommand
	AllInput
)

const (
	// SendWarning 发送警告
	SendWarning CensorHandler = iota
	// SendNotice 向通知列表/邮件发送通知
	SendNotice
	// BanUser 拉黑用户
	BanUser
	// BanGroup 拉黑群
	BanGroup
	// BanInviter 拉黑邀请人
	BanInviter
	// AddScore 增加怒气值
	AddScore
	// SendEncodedDetails 向当前会话发送 Base64 编码的命中词和上下文片段
	SendEncodedDetails
)

const maxCensorHitContextRunes = 80

var CensorHandlerText = map[CensorHandler]string{
	SendWarning:        "SendWarning",
	SendNotice:         "SendNotice",
	BanUser:            "BanUser",
	BanGroup:           "BanGroup",
	BanInviter:         "BanInviter",
	AddScore:           "AddScore",
	SendEncodedDetails: "SendEncodedDetails",
}

type CensorHandler int

type CensorManager struct {
	IsLoading           bool
	Parent              *Dice
	Censor              *censor.Censor
	DB                  engine.DatabaseOperator
	SensitiveWordsFiles map[string]*censor.WordFile
}

func (d *Dice) NewCensorManager() {
	cm := CensorManager{
		Censor: &censor.Censor{
			CaseSensitive:  d.Config.CensorCaseSensitive,
			MatchPinyin:    d.Config.CensorMatchPinyin,
			FilterRegexStr: d.Config.CensorFilterRegexStr,
		},
		DB: d.DBOperator,
	}
	cm.Parent = d
	d.CensorManager = &cm
	if d.Config.CensorThresholds == nil {
		(&d.Config).CensorThresholds = make(map[censor.Level]int)
	}
	if d.Config.CensorHandlers == nil {
		(&d.Config).CensorHandlers = make(map[censor.Level]uint8)
	}
	if d.Config.CensorScores == nil {
		(&d.Config).CensorScores = make(map[censor.Level]int)
	}
	cm.Load(d)
}

// Load 审查加载
func (cm *CensorManager) Load(d *Dice) {
	log := d.Logger
	fileDir := "./data/censor"
	cm.IsLoading = true
	cm.Censor.SensitiveKeys = make(map[string]censor.WordInfo)
	_ = os.MkdirAll(fileDir, 0o755)
	_ = filepath.Walk(fileDir, func(path string, info fs.FileInfo, err error) error {
		if !info.IsDir() && (filepath.Ext(path) == ".txt" || filepath.Ext(path) == ".toml") {
			cm.Parent.Logger.Infof("正在读取敏感词文件：%s\n", path)
			fileInfo, e := cm.Censor.PreloadFile(path)
			if e != nil {
				log.Errorf("censor: unable to read %s, %v", path, e)
			}
			if cm.SensitiveWordsFiles == nil {
				cm.SensitiveWordsFiles = make(map[string]*censor.WordFile)
			}
			cm.SensitiveWordsFiles[fileInfo.Key] = fileInfo
		}
		return nil
	})
	err := cm.Censor.Load()
	if err != nil {
		log.Errorf("censor: load fail, %v", err)
	}
	cm.IsLoading = false
}

func (cm *CensorManager) Check(ctx *MsgContext, msg *Message, checkContent string) (*MsgCheckResult, error) {
	if cm.IsLoading {
		return nil, errors.New("censor is loading")
	}
	res := cm.Censor.Check(checkContent)
	if !ctx.Censored && res.HighestLevel > censor.Ignore {
		// 敏感词命中记录保存
		service.CensorAppend(cm.DB, ctx.MessageType, msg.Sender.UserID, msg.GroupID, msg.Message, res.SensitiveWords, int(res.HighestLevel))
	}
	count := service.CensorCount(cm.DB, msg.Sender.UserID)

	var words []string
	for word := range res.SensitiveWords {
		words = append(words, word)
	}
	sort.Strings(words)
	return &MsgCheckResult{
		UserID:            msg.Sender.UserID,
		Level:             res.HighestLevel,
		HitCounts:         count,
		CurSensitiveWords: words,
	}, nil
}

type MsgCheckResult struct {
	UserID            string
	Level             censor.Level
	HitCounts         map[censor.Level]int
	CurSensitiveWords []string
}

func censorHitContext(content string, words []string) string {
	contentRunes := []rune(content)
	if len(contentRunes) <= maxCensorHitContextRunes {
		return content
	}

	contentLower := strings.ToLower(content)
	hitStart := -1
	hitLen := 0
	for _, word := range words {
		if word == "" {
			continue
		}
		byteIndex := strings.Index(contentLower, strings.ToLower(word))
		if byteIndex < 0 {
			continue
		}
		start := len([]rune(contentLower[:byteIndex]))
		if hitStart < 0 || start < hitStart {
			hitStart = start
			hitLen = len([]rune(word))
		}
	}
	if hitStart < 0 {
		return "..."
	}

	start := hitStart
	if hitLen < maxCensorHitContextRunes {
		start -= (maxCensorHitContextRunes - hitLen) / 2
	}
	start = max(0, min(start, len(contentRunes)-maxCensorHitContextRunes))
	end := start + maxCensorHitContextRunes

	context := string(contentRunes[start:end])
	if start > 0 {
		context = "..." + context
	}
	if end < len(contentRunes) {
		context += "..."
	}
	return context
}

func formatCensorHitDetails(levelText string, words []string, content string) string {
	encodedWords := make([]string, 0, len(words))
	for _, word := range words {
		encodedWords = append(encodedWords, base64.StdEncoding.EncodeToString([]byte(word)))
	}
	context := censorHitContext(content, words)

	return fmt.Sprintf(
		"检测到<%s>级敏感词。\n命中词(Base64): %s\n上下文片段(Base64): %s",
		levelText,
		strings.Join(encodedWords, " | "),
		base64.StdEncoding.EncodeToString([]byte(context)),
	)
}

func (d *Dice) CensorMsg(mctx *MsgContext, msg *Message, checkContent string, sendContent string) (hit bool, hitWords []string, needToTerminate bool, newContent string) {
	log := d.Logger
	checkResult, err := d.CensorManager.Check(mctx, msg, checkContent)
	if err != nil {
		// FIXME: 尽管这种情况比较少，但是是否要提供一个配置项，用来控制默认是跳过还是拦截吗？
		log.Warnf("拦截系统出错(%s)，来自<%s>(%s)的消息跳过了检查", err.Error(), msg.Sender.Nickname, msg.Sender.UserID)
		return hit, hitWords, needToTerminate, newContent
	}
	newContent = sendContent

	if checkResult.Level <= censor.Ignore {
		return hit, hitWords, needToTerminate, newContent
	}

	hit = true
	hitWords = checkResult.CurSensitiveWords
	// 注意：出站掩码**不在**这里做。本函数负责"写命中记录 + 计数 + 阈值 + 警告/拉黑"，
	// 而掩码走的是另一条纯匹配路径（见 CensorMaskOutgoing）。调用方先调用本函数计数、
	// 再对文本调掩码 —— 这样"掩码的同时照常计违规"才成立；
	// 帮助文档只调掩码、不经过本函数，所以不会计数、不会把骰主自己拉黑。

	if mctx.Censored {
		return hit, hitWords, needToTerminate, newContent
	}

	mctx.Censored = true
	groupInfo, ok := mctx.Session.ServiceAtNew.Load(msg.GroupID)
	if !ok {
		d.Logger.Warn("Dice CenSor获取GroupInfo失败")
	}
	thresholds := d.Config.CensorThresholds

	// 保证按程度依次降低来处理
	var tempLevels censor.Levels
	for level := range checkResult.HitCounts {
		tempLevels = append(tempLevels, level)
	}
	sort.Sort(sort.Reverse(tempLevels))

	for _, level := range tempLevels {
		hitCount := checkResult.HitCounts[level]
		if hitCount > thresholds[level] {
			// 处理完跳出，多个等级超过阈值的处理仅进行最高的处理
			// 需要终止后续动作
			needToTerminate = true
			// 清空此用户该等级计数
			service.CensorClearLevelCount(d.CensorManager.DB, msg.Sender.UserID, level)
			// 该等级敏感词超过阈值，执行操作
			handler := d.Config.CensorHandlers[level]
			levelText := censor.LevelText[level]
			if handler&(1<<SendWarning) != 0 {
				tmplText := fmt.Sprintf("核心:拦截_警告内容_%s级", censor.LevelText[level])
				ReplyToSenderNoCheck(mctx, msg, DiceFormatTmpl(mctx, tmplText))
			}
			if handler&(1<<SendEncodedDetails) != 0 {
				ReplyToSenderNoCheck(mctx, msg, formatCensorHitDetails(levelText, checkResult.CurSensitiveWords, checkContent))
			}
			if handler&(1<<SendNotice) != 0 {
				// 向通知列表/邮件发送通知
				var text string
				switch msg.MessageType {
				case "group":
					text = fmt.Sprintf(
						"群(%s)内<%s>(%s)触发<%s>敏感词拦截",
						groupInfo.GroupID,
						msg.Sender.Nickname,
						msg.Sender.UserID,
						levelText,
					)
				case "private":
					text = fmt.Sprintf(
						"<%s>(%s)触发<%s>敏感词拦截",
						msg.Sender.Nickname,
						msg.Sender.UserID,
						levelText,
					)
				}
				mctx.Notice(text, NoticeTypeCensor)
			}
			if handler&(1<<BanUser) != 0 {
				// 拉黑用户
				(&d.Config).BanList.AddScoreBase(
					msg.Sender.UserID,
					d.Config.BanList.ThresholdBan,
					"敏感词审查",
					"触发<"+levelText+">敏感词",
					mctx,
				)
			}
			if handler&(1<<BanGroup) != 0 {
				// 拉黑群
				if msg.MessageType == "group" {
					(&d.Config).BanList.AddScoreBase(
						msg.GroupID,
						d.Config.BanList.ThresholdBan,
						"敏感词审查",
						"触发<"+levelText+">敏感词",
						mctx,
					)
				}
			}
			if handler&(1<<BanInviter) != 0 {
				// 拉黑邀请人
				if msg.MessageType == "group" {
					(&d.Config).BanList.AddScoreBase(
						groupInfo.InviteUserID,
						d.Config.BanList.ThresholdBan,
						"敏感词审查",
						"触发<"+levelText+">敏感词",
						mctx,
					)
				}
			}
			if handler&(1<<AddScore) != 0 {
				score, ok := d.Config.CensorScores[level]
				if !ok {
					score = 100
				}
				// 仅增加怒气值
				if msg.MessageType == "group" {
					(&d.Config).BanList.AddScoreByCensor(
						msg.Sender.UserID,
						int64(score),
						groupInfo.GroupID,
						levelText,
						mctx,
					)
				}
			}
			// 只处理一次
			d.Logger.Infof(
				"<%s>(%s)发送的「%s」触发最高<%s>级敏感词（%s），触发次数已经超过阈值，进行处理",
				msg.Sender.Nickname,
				msg.Sender.UserID,
				msg.Message,
				censor.LevelText[level],
				strings.Join(checkResult.CurSensitiveWords, "|"),
			)
			break
		}
	}
	return hit, hitWords, needToTerminate, newContent
}

// ---------- 出站掩码 ----------

// DefaultCensorMaskChar 掩码字符的默认值。
// 命中词有几个字符就重复几次：4 字词 → 「口口口口」。
const DefaultCensorMaskChar = "口"

// censorMaskProtectedRe 出站掩码必须跳过的结构：CQ 码与海豹码。
//
// 命中词落在这两种结构内部时**不能**替换 —— 改坏一个 CQ 参数轻则图片/文件发不出去，
// 重则把整个 CQ 码截断成一段乱文本发给用户。
var censorMaskProtectedRe = regexp.MustCompile(`\[CQ:.+?]|\[(?:img|图|文本|text|语音|voice|视频|video):.+?]`)

// CensorHitWords 纯匹配一遍文本，返回命中的敏感词（按长词优先排序）。
//
// 与 CensorManager.Check 的区别：**不写命中记录、不计数、不碰怒气值/拉黑**，
// 也不受 CensorMaskEnable 影响 —— 需要"只是想看看有没有命中"的地方（出站掩码、
// 帮助文档决定要不要转图片）都用它。
func (d *Dice) CensorHitWords(text string) []string {
	if d == nil || text == "" {
		return nil
	}
	cm := d.CensorManager
	if cm == nil || cm.Censor == nil || cm.IsLoading {
		return nil
	}
	res := cm.Censor.Check(text)
	if res.HighestLevel <= censor.Ignore || len(res.SensitiveWords) == 0 {
		return nil
	}
	words := make([]string, 0, len(res.SensitiveWords))
	for word := range res.SensitiveWords {
		words = append(words, word)
	}
	// 长词优先：先替换「天主教」再替换「天主」，否则短词把长词切碎、掩码长度也算错。
	sort.Slice(words, func(i, j int) bool {
		if len(words[i]) != len(words[j]) {
			return len(words[i]) > len(words[j])
		}
		return words[i] < words[j]
	})
	return words
}

// CensorMaskOutgoing 给**骰子要发出的文本**打码：命中的敏感词替换成等长掩码。
//
// 这是"纯匹配"路径：直接问引擎 cm.Censor.Check，不经过 CensorManager.Check，
// 因此**不写命中记录、不计数、不影响怒气值与拉黑判定**。
//
// 两条调用路径的计数规则（UI 的拦截词页面里写了同样一段说明）：
//   - 帮助文档（.help / .find）只调本函数 → 掩码但不计数。否则骰主会被自己写的
//     帮助文档拉黑，而帮助文档本来就是骰主主动公开的内容。
//   - 其它出站（掷骰结果 / 自定义回复 / 合并转发）在调用本函数**之前**会先走
//     CensorMsg 计数与阈值处理 → 掩码的同时照常计违规，风控强度不变。
//
// 返回掩码后的文本，以及**实际被替换掉**的词（供日志排查）。
// 注意：命中来自拼音匹配 / 过滤字符正则时，词表里的原词可能并不字面出现在文本里，
// 那种情况下不会替换（但依然会计数），属于已知限制。
func (d *Dice) CensorMaskOutgoing(text string) (string, []string) {
	if d == nil || !d.Config.CensorMaskEnable || text == "" {
		return text, nil
	}
	words := d.CensorHitWords(text)
	if len(words) == 0 {
		return text, nil
	}
	return censorMaskWords(text, words, d.Config.CensorMaskChar)
}

// censorMaskWords 把 words 逐个替换成等长掩码，只处理**非 CQ / 海豹码**的片段。
//
// 这里自己把词按长度降序排一遍（不改调用方的切片）：先替换「天主教」再替换「天主」，
// 否则短词先把长词切碎，掩码长度也就跟着算错了。
func censorMaskWords(text string, words []string, maskChar string) (string, []string) {
	maskChar = strings.TrimSpace(maskChar)
	if maskChar == "" {
		maskChar = DefaultCensorMaskChar
	}

	sorted := make([]string, len(words))
	copy(sorted, words)
	sort.Slice(sorted, func(i, j int) bool {
		if len(sorted[i]) != len(sorted[j]) {
			return len(sorted[i]) > len(sorted[j])
		}
		return sorted[i] < sorted[j]
	})

	segments := censorMaskProtectedRe.Split(text, -1)
	protected := censorMaskProtectedRe.FindAllString(text, -1)

	var replaced []string
	for i := range segments {
		segment := segments[i]
		for _, word := range sorted {
			if word == "" || !strings.Contains(segment, word) {
				continue
			}
			mask := strings.Repeat(maskChar, utf8.RuneCountInString(word))
			segment = strings.ReplaceAll(segment, word, mask)
			replaced = append(replaced, word)
		}
		segments[i] = segment
	}
	if len(replaced) == 0 {
		return text, nil
	}

	var b strings.Builder
	b.Grow(len(text))
	for i, segment := range segments {
		b.WriteString(segment)
		if i < len(protected) {
			b.WriteString(protected[i])
		}
	}
	return b.String(), replaced
}

func (cm *CensorManager) DeleteCensorWordFiles(keys []string) {
	for _, key := range keys {
		file, ok := cm.SensitiveWordsFiles[key]
		if ok {
			_, err := os.Stat(file.Path)
			if !os.IsNotExist(err) {
				_ = os.RemoveAll(file.Path)
			}
			delete(cm.SensitiveWordsFiles, key)
		}
	}
}
