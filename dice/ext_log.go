package dice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-module/carbon"
	"github.com/pilagod/gorm-cursor-paginator/v2/paginator"
	ds "github.com/sealdice/dicescript"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"sealdice-core/dice/service"
	"sealdice-core/dice/storylog"
	"sealdice-core/model"
)

var ErrGroupCardOverlong = errors.New("群名片长度超过限制")

// logDedupWindowMaxSec 「同一条消息只记一次」的最大窗口，防止误填一个巨大的值。
const logDedupWindowMaxSec = 600

// logDedupDefaultWindowSec 默认窗口，与上游原实现保持一致（5 秒）。
const logDedupDefaultWindowSec = 5

// logExtDice 由 ext_log 注册时赋值，供闭包读取配置（RegisterExtension 的回调签名里没有 Dice）。
var logExtDice *Dice

// logDedupWindowSec 取「同一条消息只记一次」的窗口秒数，并收敛到合法范围。
func logDedupWindowSec(d *Dice) int64 {
	sec := int64(0)
	if d != nil {
		sec = d.Config.LogMultiBotDedupWindowSec
	}
	if sec <= 0 {
		sec = logDedupDefaultWindowSec
	}
	if sec > logDedupWindowMaxSec {
		sec = logDedupWindowMaxSec
	}
	return sec
}

// logCrossBotDedupEnabled 是否启用「跨连接去重」。
//
// 只有窗口被显式调大（> 5 秒）才启用，也就是使用者明确表示
// 「同一个群里我会同时挂多个 bot」。默认关闭，行为与上游一致。
func logCrossBotDedupEnabled(d *Dice) bool {
	return logDedupWindowSec(d) > logDedupDefaultWindowSec
}

func getGroupLogState(group *GroupInfo) GroupLogState {
	if group == nil {
		return GroupLogState{}
	}
	return group.GetLogState()
}

func getGroupLogName(group *GroupInfo) string {
	return getGroupLogState(group).Name
}

func getGroupLogOn(group *GroupInfo) bool {
	state := getGroupLogState(group)
	return state.On && state.Name != ""
}

func ensureGroupLogState(ctx *MsgContext, group *GroupInfo) GroupLogState {
	state := getGroupLogState(group)
	if group == nil || !state.On || state.Name == "" || state.ID > 0 {
		return state
	}

	logID, err := service.LogGetOrCreate(ctx.Dice.DBOperator, group.GroupID, state.Name)
	if err != nil {
		if ctx != nil && ctx.Dice != nil && ctx.Dice.Logger != nil {
			ctx.Dice.Logger.Warnf("日志状态修复失败: 群=%s 名称=%s err=%v", group.GroupID, state.Name, err)
		}
		return state
	}
	group.SetLogState(logID, state.Name, state.On)
	if ctx != nil && ctx.Dice != nil && ctx.Dice.Logger != nil {
		ctx.Dice.Logger.Infof("日志状态修复: 群=%s 记录=%s 补全logID=%d", group.GroupID, state.Name, logID)
	}
	return getGroupLogState(group)
}

// EvalPlayerGroupCardTemplate 只计算 .sn 名片模板，不调用平台接口改名。
// QQ 官方机器人无法修改群名片，需要把模板当作“虚拟角色状态栏”来读取，
// 因此把纯计算部分抽出来复用。
func EvalPlayerGroupCardTemplate(ctx *MsgContext, tmpl string) (string, error) {
	if ctx == nil || ctx.Dice == nil {
		return "", errors.New("上下文未初始化")
	}
	if ctx.SystemTemplate == nil {
		ctx.SystemTemplate = ctx.Group.GetCharTemplate(ctx.Dice)
	}
	config := ctx.GenDefaultRollVmConfig()
	config.HookValueStore = func(ctx *ds.Context, name string, v *ds.VMValue) (overwrite *ds.VMValue, solved bool) {
		return nil, true
	}
	v := ctx.EvalFString(tmpl, config)
	if v.vm.Error != nil {
		ctx.Dice.Logger.Infof("SN指令模板错误: %v", v.vm.Error.Error())
		return "", v.vm.Error
	}
	return v.ToString(), nil
}

// SetPlayerGroupCardByTemplate 计算模板并调用平台接口修改群名片。
func SetPlayerGroupCardByTemplate(ctx *MsgContext, tmpl string) (string, error) {
	text, err := EvalPlayerGroupCardTemplate(ctx, tmpl)
	if err != nil {
		return "", err
	}
	if ctx.EndPoint.Platform == "QQ" && len(text) >= 60 { // Note(Xiangze-Li): 2023-08-09实测群名片长度限制为59个英文字符, 20个中文字符是可行的, 但分别判断过于繁琐
		return text, ErrGroupCardOverlong
	}

	ctx.EndPoint.Adapter.SetGroupCardName(ctx, text)
	return text, nil
}

// {"data":null,"msg":"SEND_MSG_API_ERROR","retcode":100,"status":"failed","wording":"请参考 go-cqhttp 端输出"}

func RegisterBuiltinExtLog(self *Dice) {
	privateCommandListen := map[int64]int64{}
	privateCommandListenMu := sync.RWMutex{}

	// 这个机制作用是记录私聊指令？？忘记了
	privateCommandListenCheck := func() {
		now := time.Now().Unix()
		newMap := map[int64]int64{}
		privateCommandListenMu.Lock()
		for k, v := range privateCommandListen {
			// 30s间隔以上清除
			if now-v < 30 {
				newMap[k] = v
			}
		}
		privateCommandListen = newMap
		privateCommandListenMu.Unlock()
	}

	privateCommandListenHas := func(commandID int64) bool {
		privateCommandListenMu.RLock()
		_, exists := privateCommandListen[commandID]
		privateCommandListenMu.RUnlock()
		return exists
	}

	privateCommandListenSet := func(commandID int64, ts int64) {
		privateCommandListenMu.Lock()
		privateCommandListen[commandID] = ts
		privateCommandListenMu.Unlock()
	}

	// 避免群信息重复记录。
	//
	// 【默认行为 = 上游原样】窗口 5 秒、按 msg.RawID 去重。这是最保守的做法：
	// 只挡「同一个连接把同一条消息重复推送」，不会误伤任何真实发言。
	//
	// 只有当你把 logMultiBotDedupWindowSec 显式调大（> 5）时，才切换成
	// 「跨连接去重」模式：窗口按配置放宽，并且改用「群+人+正文」作为判定键。
	// 原因：同一个群里同时挂官方 bot 和民间 bot 时，同一条玩家消息会被两个连接
	// 各收一次，而各自的 msg.RawID 完全不同，按 RawID 根本挡不住。
	//
	// ⚠️ 跨连接模式的代价：同一个人在窗口内发的两条**内容完全相同**的消息会被并成一条
	// （例如连打两个「1」）。日志是永久记录，所以这个模式必须由使用者主动开启。
	// 一般人不会同时开两个 bot，那就保持默认，什么都不用管。
	groupMsgInfo := SyncMap[any, int64]{}
	groupMsgInfoLastClean := int64(0)
	groupMsgInfoWindow := func() int64 {
		return logDedupWindowSec(logExtDice)
	}
	groupMsgInfoClean := func() {
		// 清理过久的消息
		now := time.Now().Unix()
		window := groupMsgInfoWindow()
		if now-groupMsgInfoLastClean < window {
			// 一个窗口清理一次即可
			return
		}

		groupMsgInfoLastClean = now
		var toDelete []any
		groupMsgInfo.Range(func(key any, t int64) bool {
			if now-t > window {
				toDelete = append(toDelete, key)
			}
			return true
		})

		for _, i := range toDelete {
			groupMsgInfo.Delete(i)
		}
	}

	// 检查是否已经记录过 如果记录过则跳过
	groupMsgInfoCheckOk := func(_k interface{}) bool {
		groupMsgInfoClean()
		if _k == nil {
			return false
		}
		t, exists := groupMsgInfo.Load(_k)
		if exists {
			now := time.Now().Unix()
			return now-t > groupMsgInfoWindow()
		}
		return true
	}

	groupMsgInfoSet := func(_k any) {
		if _k != nil {
			groupMsgInfo.Store(_k, time.Now().Unix())
		}
	}

	// logDedupKey 生成「同一条消息」的判定键。
	//
	// 默认（窗口为 5 秒）直接用 msg.RawID，与上游行为一致：
	// 只去重同一连接内的重复推送，绝不合并真实发言。
	//
	// ⚠️ 已知限制：窗口调大后这里用的是**真实**群号与发送者号
	// （官方侧 OpenQQ:… / 民间侧 QQ:…）。同一条消息经两条连接各收一次时，
	// 两边算出的键并不相同，所以「跨 bot 去重」实际上不会命中；
	// 能命中的只有同一连接被重复推送的情况。真正的代价是：
	// 同一个人在窗口内发的两条**正文完全相同**的消息只会留下一条。
	// 想真正跨 bot 去重，键必须改用归一后的身份
	// （identityBindDataGroupID / identityBindDataUserID），见 TODO。
	// TODO(identity-bind): 用归一 ID 重做跨连接去重，否则应删掉这个开关、
	// 把窗口钉死 5 秒，避免"以为开了其实没开，却付了合并代价"。
	logDedupKey := func(ctx *MsgContext, msg *Message) any {
		if msg == nil {
			return nil
		}
		if !logCrossBotDedupEnabled(logExtDice) {
			// 保守路径：RawID 为空时退回内容键，否则一条都记不上。
			if msg.RawID != nil {
				return msg.RawID
			}
		}
		groupID := msg.GroupID
		userID := msg.Sender.UserID
		if ctx != nil && ctx.Group != nil && groupID == "" {
			groupID = ctx.Group.GroupID
		}
		if ctx != nil && ctx.Player != nil && userID == "" {
			userID = ctx.Player.UserID
		}
		if groupID == "" && userID == "" && msg.Message == "" {
			return msg.RawID
		}
		return groupID + "\x00" + userID + "\x00" + msg.Message
	}

	// 获取logname，第一项是默认名字
	getLogName := func(ctx *MsgContext, _ *Message, cmdArgs *CmdArgs, index int) (string, string) {
		bakLogCurName := getGroupLogName(ctx.Group)
		if newName := cmdArgs.GetArgN(index); newName != "" {
			return bakLogCurName, newName
		}
		return bakLogCurName, bakLogCurName
	}

	const helpLog = `.log new [<日志名>] // 新建日志并开始记录，注意new后跟空格！
.log on [<日志名>]  // 开始记录，不写日志名则开启最近一次日志，注意on后跟空格！
.log off // 暂停记录
.log end // 完成记录并发送日志文件
.log get [<日志名>] // 重新上传日志，并获取链接
.log halt // 强行关闭当前log，不上传日志
.log list // 查看当前群的日志列表
.log del <日志名> // 删除一份日志
.log stat [<日志名>] // 查看统计
.log stat [<日志名>] --all // 查看统计(全团)，--all前必须有空格
.log list <群号> // 查看指定群的日志列表(无法取得日志时，找骰主做这个操作)
.log masterget <群号> <日志名> // 重新上传日志，并获取链接(无法取得日志时，找骰主做这个操作)
.log export <日志名> // 直接取得日志txt(服务出问题或有其他需要时使用)
.log export <日志名> <邮箱地址> // 通过邮件取得日志txt，多个邮箱用空格隔开
.log bind <旧群号> // 官方机器人：把当前群的日志读取指向迁移前的旧群
.log unbind // 官方机器人：解除当前群的日志绑定
.log bindstatus // 官方机器人：查看当前群的日志绑定`

	// const txtLogTip = "若未出现线上日志地址，可换时间获取，或联系骰主在data/default/log-exports路径下取出日志\n文件名: 群号_日志名_随机数.zip\n注意此文件log end/get后才会生成"

	cmdLog := &CmdItemInfo{
		Name:      "log",
		ShortHelp: helpLog,
		Help:      "日志指令:\n" + helpLog,
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			group := ctx.Group
			// stateGroup 是「日志状态存在哪个群对象上」。
			//
			// 做了群绑定之后，官方群和旧群必须共用同一份日志状态（开没开、叫什么名字、
			// logID 是多少），否则两边会各自开一份日志、互相看不到。
			// 这里把状态统一挂在归一后的群上，读写天然同步。
			//
			// 注意不能把 group 整个换掉：group.IsActive / MarkDirty 属于「当前群自己」的
			// 状态，换掉会让 .bot on 检测错群。
			stateGroup := group
			if readGroup, bound := identityBindReadGroup(ctx); bound && readGroup != nil {
				stateGroup = readGroup
			}
			cmdArgs.ChopPrefixToArgsWith("on", "off", "del", "rm", "masterget",
				"get", "end", "halt", "list", "new", "stat", "export", "bind", "unbind", "bindstatus")

			groupNotActiveCheck := func() bool {
				if !group.IsActive(ctx) {
					ReplyToSender(ctx, msg, "未开启时不会记录日志，请先.bot on")
					return true
				}
				return false
			}

			if len(cmdArgs.Args) == 0 {
				onText := "关闭"
				state := getGroupLogState(stateGroup)
				if state.On {
					onText = "开启"
				}
				// 条数必须按「记录实际所在的那个群」去查。
				// 群绑定之后日志记录都在旧群名下，用当前群的 ID 去数必然是 0。
				lines, _ := service.LogLinesCountGet(ctx.Dice.DBOperator, stateGroup.GroupID, state.Name)
				text := fmt.Sprintf("当前故事: %s\n当前状态: %s\n已记录文本%d条", state.Name, onText, lines)
				text += identityBindStatusSuffix(ctx)
				ReplyToSender(ctx, msg, text)
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			logNameAliasIndexCache := map[string]*logNameAliasIndex{}
			getLogNameAliasIndex := func(groupID string) (*logNameAliasIndex, error) {
				if index, exists := logNameAliasIndexCache[groupID]; exists {
					return index, nil
				}
				index, err := buildLogNameAliasIndex(ctx.Dice.DBOperator, groupID)
				if err != nil {
					return nil, err
				}
				logNameAliasIndexCache[groupID] = index
				return index, nil
			}

			resolveLogNameWithReply := func(groupID, name string) (string, bool) {
				index, err := getLogNameAliasIndex(groupID)
				if err != nil {
					ReplyToSender(ctx, msg, "获取记录出错: "+err.Error())
					return "", false
				}
				if resolved, ok := index.Resolve(name); ok {
					return resolved, true
				}
				return name, true
			}

			logKeyHintText := func() string {
				return "\n可先使用 .log list 查看对应的【key】: 日志名"
			}

			getAndUpload := func(gid, lname string) {
				if lname != "" {
					var ok bool
					lname, ok = resolveLogNameWithReply(gid, lname)
					if !ok {
						return
					}
				}
				unofficial, fn, notice, err := logSendToBackend(ctx, gid, lname, true)
				if err != nil {
					reason := strings.TrimPrefix(err.Error(), "#")
					VarSetValueStr(ctx, "$t错误原因", reason)

					tmpl := DiceFormatTmpl(ctx, "日志:记录_上传_失败")
					if strings.Contains(reason, "此log不存在") || strings.Contains(reason, "名字是否正确") {
						tmpl += logKeyHintText()
					}
					ReplyToSenderRaw(ctx, msg, tmpl, "skip")
				} else {
					VarSetValueStr(ctx, "$t日志链接", fn)
					tmpl := DiceFormatTmpl(ctx, "日志:记录_上传_成功")
					if unofficial {
						tmpl += "\n[注意：该链接非海豹官方染色器]"
					}
					if notice != "" {
						tmpl += "\n" + notice
					}
					ReplyToSenderRaw(ctx, msg, tmpl, "skip")
				}
			}

			// QQ 官方机器人的日志绑定：.log bind / .log unbind / .log bindstatus
			if result, handled := runIdentityBindLogCommand(ctx, msg, cmdArgs); handled {
				return result
			}

			if cmdArgs.IsArgEqual(1, "on") { //nolint:nestif
				if ctx.IsPrivate {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				// 如果日志已经开启，报错返回
				currentState := getGroupLogState(stateGroup)
				if currentState.On {
					VarSetValueStr(ctx, "$t记录名称", currentState.Name)
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_开启_失败_未结束的记录"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				name := cmdArgs.GetArgN(2)
				if name == "" {
					name = currentState.Name
				}
				if name != "" {
					var ok bool
					name, ok = resolveLogNameWithReply(stateGroup.GroupID, name)
					if !ok {
						return CmdExecuteResult{Matched: true, Solved: true}
					}
				}

				if name != "" {
					lines, exists := service.LogLinesCountGet(ctx.Dice.DBOperator, stateGroup.GroupID, name)

					if exists {
						if groupNotActiveCheck() {
							return CmdExecuteResult{Matched: true, Solved: true}
						}

						logID, err := service.LogGetOrCreate(ctx.Dice.DBOperator, stateGroup.GroupID, name)
						if err != nil {
							ReplyToSender(ctx, msg, "日志开启失败: "+err.Error())
							ctx.Dice.Logger.Errorf("日志开启失败: group=%s name=%s err=%v", stateGroup.GroupID, name, err)
							return CmdExecuteResult{Matched: true, Solved: true}
						}
						stateGroup.SetLogState(logID, name, true)
						stateGroup.MarkDirty(ctx.Dice)
						ctx.Dice.Logger.Infof("日志状态切换: 群=%s 开启日志 name=%s id=%d", stateGroup.GroupID, name, logID)

						VarSetValueStr(ctx, "$t记录名称", name)
						VarSetValueInt64(ctx, "$t当前记录条数", lines)
						ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_开启_成功"))
					} else {
						VarSetValueStr(ctx, "$t记录名称", name)
						ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_开启_失败_无此记录"))
					}
				} else {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_开启_失败_尚未新建"))
				}
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "off") {
				state := getGroupLogState(stateGroup)
				if state.Name != "" && state.On {
					stateGroup.SetLogOn(false)
					stateGroup.MarkDirty(ctx.Dice)
					ctx.Dice.Logger.Infof("日志状态切换: 群=%s 暂停日志 name=%s id=%d", stateGroup.GroupID, state.Name, state.ID)
					lines, _ := service.LogLinesCountGet(ctx.Dice.DBOperator, stateGroup.GroupID, state.Name)
					VarSetValueStr(ctx, "$t记录名称", state.Name)
					VarSetValueInt64(ctx, "$t当前记录条数", lines)
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_关闭_成功"))
				} else {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_关闭_失败"))
				}
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "del", "rm") {
				name := cmdArgs.GetArgN(2)
				if name == "" {
					return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
				}
				// 别名解析与真正的删除都必须落在**归一后**的群上：
				// 群绑定之后日志行在旧群名下，用真实群去解析会一直"找不到"；
				// 而官方群如果恰好有一份同名旧日志（绑定时被隐藏的那份），
				// 删掉的就是它 —— 连 log_items 一起没了，还回一句"删除成功"。
				delGroupID := identityBindLogReadGroupID(ctx, stateGroup.GroupID)
				var ok bool
				name, ok = resolveLogNameWithReply(delGroupID, name)
				if !ok {
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				VarSetValueStr(ctx, "$t记录名称", name)
				// 「正在进行的记录」也要按归一后的群判断，否则群绑定之后
				// 会把正在记录的那一份当成普通记录删掉。
				if name == getGroupLogName(stateGroup) {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_删除_失败_正在进行"))
				} else {
					err := service.LogDelete(ctx.Dice.DBOperator, delGroupID, name)
					if err == nil {
						ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_删除_成功"))
					} else if errors.Is(err, service.ErrLogNotFound) {
						ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_删除_失败_找不到"))
					} else {
						ReplyToSender(ctx, msg, "日志删除失败: "+err.Error())
					}
				}
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "masterget") {
				groupID, requestForAnotherGroup := getSpecifiedGroupIfMaster(ctx, msg, cmdArgs)
				if requestForAnotherGroup && groupID == "" {
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				logName := cmdArgs.GetArgN(3)
				if logName == "" {
					ReplyToSenderRaw(ctx, msg, "请遵循 .log masterget <群号> <日志名> 格式给出日志名，注意空格\n若不清楚可以.log list <群号>查询", "skip")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				getAndUpload(groupID, logName)
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "get") {
				// 仿照 log on 为 log get 添加了 IsPrivate 判断
				if ctx.IsPrivate {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				// 当前记录名要从**归一后的群**上读：群绑定之后状态挂在旧群，
				// 真实群对象上是空的，读 group 会误判成"没有开启状态的记录"。
				logName := getGroupLogName(stateGroup)
				if newName := cmdArgs.GetArgN(2); newName != "" {
					logName = newName
				}

				if logName == "" {
					text := DiceFormatTmpl(ctx, "日志:记录_取出_未指定记录")
					ReplyToSenderRaw(ctx, msg, text, "skip")
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				getAndUpload(identityBindLogReadGroupID(ctx, group.GroupID), logName)
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "end") {
				state := getGroupLogState(stateGroup)
				if state.Name == "" {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_关闭_失败"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				lines, _ := service.LogLinesCountGet(ctx.Dice.DBOperator, stateGroup.GroupID, state.Name)
				VarSetValueInt64(ctx, "$t当前记录条数", lines)
				VarSetValueStr(ctx, "$t记录名称", state.Name)
				text := DiceFormatTmpl(ctx, "日志:记录_结束")
				// Note: 2024-02-28 经过讨论，日志在 off 的情况下 end 属于合理操作，这里不再检查是否开启
				// if !group.LogOn {
				//	 text = strings.TrimRightFunc(DiceFormatTmpl(ctx, "日志:记录_关闭_失败"), unicode.IsSpace) + "\n" + text
				// }
				ReplyToSender(ctx, msg, text)
				stateGroup.SetLogOn(false)
				stateGroup.MarkDirty(ctx.Dice)
				ctx.Dice.Logger.Infof("日志状态切换: 群=%s 结束日志 name=%s id=%d，准备上传", group.GroupID, state.Name, state.ID)

				time.Sleep(time.Duration(0.3 * float64(time.Second)))
				// Note: 2024-10-15 经过简单测试，似乎能缓解#1034的问题，但无法根本解决。
				//
				// 上传目标必须是**归一后**的群：群绑定之后日志行与条目都在旧群名下，
				// 用真实群去查只会得到"此log不存在"；更糟的是官方群如果恰好有一份
				// 同名旧日志，会把那份旧内容当成刚结束的记录上传上去
				// （它若上传过，还会直接返回当年的缓存链接，看起来像成功了）。
				uploadGroupID := identityBindLogReadGroupID(ctx, stateGroup.GroupID)
				uploadName := state.Name
				go getAndUpload(uploadGroupID, uploadName)
				stateGroup.ClearLogState()
				stateGroup.MarkDirty(ctx.Dice)
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "halt") {
				state := getGroupLogState(stateGroup)
				if len(state.Name) > 0 {
					lines, _ := service.LogLinesCountGet(ctx.Dice.DBOperator, stateGroup.GroupID, state.Name)
					VarSetValueInt64(ctx, "$t当前记录条数", lines)
					VarSetValueStr(ctx, "$t记录名称", state.Name)
				}
				text := DiceFormatTmpl(ctx, "日志:记录_结束")
				ReplyToSender(ctx, msg, text)
				stateGroup.ClearLogState()
				stateGroup.MarkDirty(ctx.Dice)
				ctx.Dice.Logger.Infof("日志状态切换: 群=%s 强制终止当前日志", group.GroupID)
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "list") {
				groupID, requestForAnotherGroup := getSpecifiedGroupIfMaster(ctx, msg, cmdArgs)
				if requestForAnotherGroup && groupID == "" {
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				if groupID == "" {
					groupID = ctx.Group.GroupID
				}
				// QQ 官方机器人做过日志绑定时，列出的是旧群的日志
				groupID = identityBindLogReadGroupID(ctx, groupID)

				var text strings.Builder
				text.WriteString(DiceFormatTmpl(ctx, "日志:记录_列出_导入语"))
				text.WriteString("\n")
				index, err := getLogNameAliasIndex(groupID)
				if err == nil {
					for _, entry := range index.entries {
						text.WriteString(formatLogNameListLine(entry))
						text.WriteString("\n")
					}
					if len(index.entries) == 0 {
						text.WriteString("暂无记录")
					}
				} else {
					text.WriteString("获取记录出错: ")
					text.WriteString(err.Error())
				}
				ReplyToSender(ctx, msg, text.String())
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "new") {
				if ctx.IsPrivate {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				name := cmdArgs.GetArgN(2)
				currentState := getGroupLogState(stateGroup)
				if currentState.Name != "" && name == "" {
					VarSetValueStr(ctx, "$t记录名称", currentState.Name)
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_新建_失败_未结束的记录"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				if groupNotActiveCheck() {
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				if name == "" {
					name = time.Now().Format("2006_01_02_15_04_05")
				}
				if currentState.Name != "" {
					VarSetValueInt64(ctx, "$t存在开启记录", 1)
				} else {
					VarSetValueInt64(ctx, "$t存在开启记录", 0)
				}
				VarSetValueStr(ctx, "$t上一记录名称", currentState.Name)
				VarSetValueStr(ctx, "$t记录名称", name)
				// 日志行必须建在「记录实际所在的那个群」上（群绑定后是旧群）。
				// 否则会出现 logID 属于旧群、group_id 却写成官方群的错配行，
				// 之后按 (group_id, name) 写入就查不到它，表现为"记不进去"。
				logID, err := service.LogGetOrCreate(ctx.Dice.DBOperator, stateGroup.GroupID, name)
				if err != nil {
					ReplyToSender(ctx, msg, "日志新建失败: "+err.Error())
					ctx.Dice.Logger.Errorf("日志新建失败: group=%s name=%s err=%v", stateGroup.GroupID, name, err)
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				stateGroup.SetLogState(logID, name, true)
				stateGroup.MarkDirty(ctx.Dice)
				ctx.Dice.Logger.Infof("日志状态切换: 群=%s 新建并开启日志 name=%s id=%d", stateGroup.GroupID, name, logID)

				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_新建"))
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "stat") {
				// group := ctx.Group
				_, name := getLogName(ctx, msg, cmdArgs, 2)
				// QQ 官方机器人做过日志绑定时，统计的是旧群的日志
				statGroupID := identityBindLogReadGroupID(ctx, group.GroupID)
				if name != "" {
					var ok bool
					name, ok = resolveLogNameWithReply(statGroupID, name)
					if !ok {
						return CmdExecuteResult{Matched: true, Solved: true}
					}
				}
				items, err := service.LogGetCommandInfoStrList(ctx.Dice.DBOperator, statGroupID, name)
				if err == nil && len(items) > 0 {
					// showDetail := cmdArgs.GetKwarg("detail")
					// var showDetail *Kwarg
					showAll := cmdArgs.GetKwarg("all")

					/* if showDetail != nil { //nolint // 故意保留
						results := LogRollBriefDetail(items)

						if len(results) > 0 {
							ReplyToSender(ctx, msg, "统计结果如下:\n"+strings.Join(results, "\n"))
							return CmdExecuteResult{Matched: true, Solved: true}
						}
					} else */{
						isShowAll := showAll != nil
						statPlayerName := ctx.Player.Name
						if boundPlayer := identityBindReadPlayer(ctx); boundPlayer != nil && boundPlayer.Name != "" {
							statPlayerName = boundPlayer.Name
						}
						text := LogRollBriefByPCV2(ctx, items, isShowAll, statPlayerName)
						if text == "" {
							if isShowAll {
								ReplyToSender(ctx, msg, fmt.Sprintf("没有找到故事“%s”的检定记录", name))
							} else {
								ReplyToSender(ctx, msg, fmt.Sprintf("没有找到角色<%s>的任何记录\n若需查看全团，请在指令后加 --all", statPlayerName))
							}
						} else {
							if !isShowAll {
								text += "\n\n若需查看全团，请在指令后加 --all"
							}
							ReplyToSender(ctx, msg, text)
						}
						return CmdExecuteResult{Matched: true, Solved: true}
					}
				}
				ReplyToSender(ctx, msg, "没有发现可供统计的信息，请确保记录名正确，且有进行骰点/检定行为")
				return CmdExecuteResult{Matched: true, Solved: true}
			} else if cmdArgs.IsArgEqual(1, "export") {
				if ctx.IsPrivate {
					// 仿照 log on 为 log get 添加了 IsPrivate 判断
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "核心:提示_私聊不可用"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				logName := getGroupLogName(stateGroup)
				if newName := cmdArgs.GetArgN(2); newName != "" {
					logName = newName
				}
				if logName == "" {
					ReplyToSenderRaw(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_导出_未指定记录"), "skip")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				var ok bool
				logName, ok = resolveLogNameWithReply(identityBindLogReadGroupID(ctx, group.GroupID), logName)
				if !ok {
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				now := carbon.Now()
				VarSetValueStr(ctx, "$t记录名", logName)
				VarSetValueStr(ctx, "$t日期", now.ToShortDateString())
				VarSetValueStr(ctx, "$t时间", now.ToShortTimeString())
				logFileNamePrefix := DiceFormatTmpl(ctx, "日志:记录_导出_文件名前缀")
				logFile, notice, err := GetLogTxt(ctx, identityBindLogReadGroupID(ctx, group.GroupID), logName, logFileNamePrefix)
				if err != nil {
					reply := err.Error()
					if strings.Contains(reply, "此log不存在") || strings.Contains(reply, "名字是否正确") {
						reply += logKeyHintText()
					}
					ReplyToSenderRaw(ctx, msg, reply, "skip")
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				defer os.Remove(logFile)

				var emails []string
				if len(cmdArgs.Args) > 2 {
					emails = cmdArgs.Args[2:]
					// 试图发送邮件
					dice := ctx.Session.Parent
					if dice.CanSendMail() {
						rightEmails := make([]string, 0, len(emails))
						emailExp := regexp.MustCompile(`.*@.*`)
						for _, email := range emails {
							if emailExp.MatchString(email) {
								rightEmails = append(rightEmails, email)
							}
						}
						if len(rightEmails) > 0 {
							emailMsg := DiceFormatTmpl(ctx, "日志:记录_导出_邮件附言")
							// SendMailRow 返回 SMTP 错误时**不能**再回"已发送"：
							// 临时导出文件在这段代码之后就被删掉了，用户既没收到邮件
							// 也没法从别处拿回日志，等于日志白导出一场。
							if err := dice.SendMailRow(
								fmt.Sprintf("Seal 记录提取: %s", logFileNamePrefix),
								rightEmails,
								emailMsg,
								[]string{logFile},
							); err != nil {
								dice.Logger.Errorf("导出日志的邮件发送失败: %v", err)
								ReplyToSenderRaw(ctx, msg, fmt.Sprintf(
									"邮件发送失败：%v\n请检查「邮箱通知」配置（发件邮箱 / 密钥 / SMTP），或改用不加邮箱参数的导出方式。",
									err), "skip")
								return CmdExecuteResult{Matched: true, Solved: true}
							}
							text := DiceFormatTmpl(ctx, "日志:记录_导出_邮箱发送前缀") + strings.Join(rightEmails, "\n")
							ReplyToSenderRaw(ctx, msg, text, "skip")
							return CmdExecuteResult{Matched: true, Solved: true}
						}
						ReplyToSenderRaw(ctx, msg, DiceFormatTmpl(ctx, "日志:记录_导出_无格式有效邮箱"), "skip")
					}
					ReplyToSenderRaw(ctx, msg, DiceFormat(ctx, "{核心:骰子名字}未配置邮箱，将直接发送记录文件"), "skip")
				}

				var uri string
				if runtime.GOOS == "windows" {
					uri = "files:///" + logFile
				} else {
					uri = "files://" + logFile
				}
				SendFileToSenderRaw(ctx, msg, uri, "skip")
				VarSetValueStr(ctx, "$t文件名字", logFileNamePrefix)
				reply := DiceFormatTmpl(ctx, "日志:记录_导出_成功")
				if notice != "" {
					reply += "\n" + notice
				}
				ReplyToSenderRaw(ctx, msg, reply, "skip")
				return CmdExecuteResult{Matched: true, Solved: true}
			} else {
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}
		},
	}

	helpStat := `.stat log [<日志名>] // 查看当前或指定日志的骰点统计
.stat log [<日志名>] --all // 查看全团
.stat help // 帮助
`
	cmdStat := &CmdItemInfo{
		Name:      "stat",
		ShortHelp: helpStat,
		Help:      "查看统计:\n" + helpStat,
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			val := cmdArgs.GetArgN(1)
			switch strings.ToLower(val) {
			case "log":
				group := ctx.Group
				// QQ 官方机器人做过日志绑定时，统计的是旧群的日志
				statGroupID := identityBindLogReadGroupID(ctx, group.GroupID)
				_, name := getLogName(ctx, msg, cmdArgs, 2)
				if name != "" {
					resolved, err := resolveLogNameForGroup(ctx.Dice.DBOperator, statGroupID, name)
					if err != nil {
						ReplyToSender(ctx, msg, "获取记录出错: "+err.Error())
						return CmdExecuteResult{Matched: true, Solved: true}
					}
					name = resolved
				}
				items, err := service.LogGetCommandInfoStrList(ctx.Dice.DBOperator, statGroupID, name)
				if err == nil && len(items) > 0 {
					// showDetail := cmdArgs.GetKwarg("detail")
					// var showDetail *Kwarg
					showAll := cmdArgs.GetKwarg("all")

					/* if showDetail != nil { //nolint // 故意保留
						results := LogRollBriefDetail(items)

						if len(results) > 0 {
							ReplyToSender(ctx, msg, "统计结果如下:\n"+strings.Join(results, "\n"))
							return CmdExecuteResult{Matched: true, Solved: true}
						}
					} else */{
						isShowAll := showAll != nil
						// 与 .log stat 保持一致：绑定了的话，日志里存的是旧号那边的昵称
						statPlayerName := ctx.Player.Name
						if boundPlayer := identityBindReadPlayer(ctx); boundPlayer != nil && boundPlayer.Name != "" {
							statPlayerName = boundPlayer.Name
						}
						text := LogRollBriefByPCV2(ctx, items, isShowAll, statPlayerName)
						if text == "" {
							if isShowAll {
								ReplyToSender(ctx, msg, fmt.Sprintf("没有找到故事“%s”的检定记录", name))
							} else {
								ReplyToSender(ctx, msg, fmt.Sprintf("没有找到角色<%s>的任何记录\n若需查看全团，请在指令后加 --all", statPlayerName))
							}
						} else {
							if !isShowAll {
								text += "\n\n若需查看全团，请在指令后加 --all"
							}
							ReplyToSender(ctx, msg, text)
						}
						return CmdExecuteResult{Matched: true, Solved: true}
					}
				}
				if err != nil || len(items) == 0 {
					ReplyToSender(ctx, msg, "没有发现可供统计的信息，请确保记录名正确，且有进行骰点/检定行为")
				}
			default:
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}
			return CmdExecuteResult{Matched: true, Solved: true}
		},
	}

	helpOb := `.ob // 进入ob模式
.ob exit // 退出ob
`
	cmdOb := &CmdItemInfo{
		Name:          "ob",
		ShortHelp:     helpOb,
		Help:          "观众指令:\n" + helpOb,
		AllowDelegate: true,
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			ctx.DelegateText = fmt.Sprintf("由<%s>操作:\n", ctx.Player.Name)
			mctx := GetCtxProxyFirst(ctx, cmdArgs)
			subcommand := cmdArgs.GetArgN(1)

			c := ctx
			if mctx != nil && mctx.Player.UserID != ctx.Player.UserID {
				if ctx.PrivilegeLevel < 50 && subcommand != "help" {
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "通用:提示_无权限_非master/管理"))
					return CmdExecuteResult{Matched: true, Solved: true}
				}
				c = mctx
			}

			switch strings.ToLower(subcommand) {
			case "help":
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			case "exit":
				if strings.HasPrefix(strings.ToLower(c.Player.Name), "ob") {
					c.Player.Name = c.Player.Name[len("ob"):]
					c.Player.UpdatedAtTime = time.Now().Unix()
					if c.Group != nil {
						c.Group.MarkDirty(c.Dice)
					}
				}
				c.EndPoint.Adapter.SetGroupCardName(c, c.Player.Name)
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:OB_关闭"))
			default:
				if !strings.HasPrefix(strings.ToLower(c.Player.Name), "ob") {
					c.Player.Name = "ob" + c.Player.Name
					c.Player.UpdatedAtTime = time.Now().Unix()
					if c.Group != nil {
						c.Group.MarkDirty(c.Dice)
					}
				}
				c.EndPoint.Adapter.SetGroupCardName(c, c.Player.Name)
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:OB_开启"))
			}
			return CmdExecuteResult{Matched: true, Solved: true}
		},
	}

	helpSn := `.sn coc // 自动设置coc名片
.sn dnd // 自动设置dnd名片
.sn none // 设置为空白格式
.sn off // 取消自动设置
`
	cmdSn := &CmdItemInfo{
		Name:               "sn",
		ShortHelp:          helpSn,
		Help:               "跑团名片(需要管理权限):\n" + helpSn,
		CheckCurrentBotOn:  true,
		CheckMentionOthers: true,
		HelpFunc: func(isShort bool) string {
			// 手动添加特定的命令示例到帮助信息的开头
			fixedExamples := ".sn coc // 自动设置coc名片\n" +
				".sn cocL // 自动设置coc名片，小写\n" +
				".sn dnd // 自动设置dnd名片\n"

			text := fixedExamples

			var tempStrList []string

			self.GameSystemMap.Range(func(key string, value *GameSystemTemplate) bool {
				for k, v := range value.NameTemplate {
					if k != "coc" && k != "dnd" && k != "cocL" {
						// 考虑到这里的量级不会太大，所以直接排序已经生成好的提示文本或许更划算
						tempStrList = append(tempStrList, fmt.Sprintf(".sn %s // %s\n", k, v.HelpText))
					}
				}
				return true
			})

			sort.Strings(tempStrList)
			text += strings.Join(tempStrList, "")
			text += ".sn expr {$t玩家_RAW} HP{hp}/{hpmax} // 自设格式\n" +
				".sn none // 设置为空白格式\n" +
				".sn off // 取消自动设置"

			if isShort {
				return text
			}
			return "跑团名片(需要管理权限):\n" + text
		},
		Solve: func(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) CmdExecuteResult {
			val := cmdArgs.GetArgN(1)
			valLower := strings.ToLower(val)

			// currentSnTemplate 是「当前实际生效」的模板：自己设过就用自己那份，
			// 没设过则回退到绑定另一侧（例如官方号沿用旧 QQ 号设好的名片格式）。
			// 注意这里只用于**读取**；写入仍然落在 ctx.Player 上，避免官方侧的
			// .sn 去改动旧群的群名片。
			currentSnTemplate := identityBindPlayerNameTemplate(ctx)

			handleOverlong := func(ctx *MsgContext, msg *Message, card string) CmdExecuteResult {
				ReplyToSender(ctx, msg, fmt.Sprintf(
					"尝试将群名片修改为 %q 失败，名片长度超过限制。\n请尝试缩短角色名或使用 .sn expr 自定义名片格式。",
					card,
				))
				return CmdExecuteResult{Matched: true, Solved: true}
			}

			// sn 指令(除 help 外)需要骰子具有群管理员权限。提前检测 bot 在群中的角色,
			// 若明确无管理权限则给出明确提示, 由于协议端问题，目前只能做提示。不支持角色检查的适配器
			// 返回 ok=false, 此时保持原有行为(直接尝试设置)不做阻断。
			if valLower != "help" && ctx.Group != nil {
				if detail, ok := checkBotGroupRole(ctx, ctx.Group.GroupID); ok && detail != "owner" && detail != "admin" {
					ReplyToSender(ctx, msg, "【警告】骰子当前可能不具备管理员权限，请检查，若确认具备管理员权限可无视此误报。")
				}
			}

			switch valLower {
			case "help":
				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			case "coc", "coc7":
				ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} SAN{理智} HP{生命值}/{生命值上限} DEX{敏捷}"
				identityBindNoteNameTemplate(ctx, false)
				ctx.Player.UpdatedAtTime = time.Now().Unix()
				if ctx.Group != nil {
					ctx.Group.MarkDirty(ctx.Dice)
				}
				text, err := SetPlayerGroupCardByTemplate(ctx, ctx.Player.AutoSetNameTemplate)
				if errors.Is(err, ErrGroupCardOverlong) {
					return handleOverlong(ctx, msg, text)
				}
				VarSetValueStr(ctx, "$t名片格式", val)
				VarSetValueStr(ctx, "$t名片预览", text)
				// 玩家 SAN60 HP10/10 DEX65
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_自动设置"))
			case "dnd", "dnd5e":
				// PW{pw}
				ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW} HP{hp}/{hpmax} AC{ac} DC{dc} PP{pp}"
				identityBindNoteNameTemplate(ctx, false)
				ctx.Player.UpdatedAtTime = time.Now().Unix()
				if ctx.Group != nil {
					ctx.Group.MarkDirty(ctx.Dice)
				}
				text, err := SetPlayerGroupCardByTemplate(ctx, ctx.Player.AutoSetNameTemplate)
				if errors.Is(err, ErrGroupCardOverlong) {
					return handleOverlong(ctx, msg, text)
				}
				VarSetValueStr(ctx, "$t名片格式", val)
				VarSetValueStr(ctx, "$t名片预览", text)
				// 玩家 HP10/10 AC15 DC15 PW10
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_自动设置"))
			case "none":
				ctx.Player.AutoSetNameTemplate = "{$t玩家_RAW}"
				identityBindNoteNameTemplate(ctx, false)
				ctx.Player.UpdatedAtTime = time.Now().Unix()
				if ctx.Group != nil {
					ctx.Group.MarkDirty(ctx.Dice)
				}
				text, err := SetPlayerGroupCardByTemplate(ctx, "{$t玩家_RAW}")
				if errors.Is(err, ErrGroupCardOverlong) { // 大约不至于会走到这里，但是为了统一也这样写了
					return handleOverlong(ctx, msg, text)
				}
				VarSetValueStr(ctx, "$t名片格式", "空白")
				VarSetValueStr(ctx, "$t名片预览", text)
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_自动设置"))
			case "off", "cancel":
				_, _ = SetPlayerGroupCardByTemplate(ctx, "{$t玩家_RAW}")
				ctx.Player.AutoSetNameTemplate = ""
				// 明确记下"关掉了"：否则绑定另一侧的模板又会被捡回来，
				// 状态栏看起来关不掉。
				identityBindNoteNameTemplate(ctx, true)
				ctx.Player.UpdatedAtTime = time.Now().Unix()
				if ctx.Group != nil {
					ctx.Group.MarkDirty(ctx.Dice)
				}
				ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_取消设置"))
			case "expr":
				t := cmdArgs.GetRestArgsFrom(2)
				if len(t) > 80 {
					t = t[:80]
				}
				if t == "" {
					_, _ = SetPlayerGroupCardByTemplate(ctx, "{$t玩家_RAW}")
					ctx.Player.AutoSetNameTemplate = ""
					identityBindNoteNameTemplate(ctx, true)
					ctx.Player.UpdatedAtTime = time.Now().Unix()
					if ctx.Group != nil {
						ctx.Group.MarkDirty(ctx.Dice)
					}
					ReplyToSender(ctx, msg, "玩家自设内容为空，已自动关闭此功能")
				} else {
					last := currentSnTemplate
					ctx.Player.AutoSetNameTemplate = t
					text, err := SetPlayerGroupCardByTemplate(ctx, ctx.Player.AutoSetNameTemplate)
					if err != nil && !errors.Is(err, ErrGroupCardOverlong) {
						ctx.Player.AutoSetNameTemplate = last
						ReplyToSender(ctx, msg, "玩家自设sn格式错误，已自动还原之前模板")
					} else if errors.Is(err, ErrGroupCardOverlong) {
						return handleOverlong(ctx, msg, text)
					} else {
						ctx.Player.UpdatedAtTime = time.Now().Unix()
						identityBindNoteNameTemplate(ctx, false)
						if ctx.Group != nil {
							ctx.Group.MarkDirty(ctx.Dice)
						}
						VarSetValueStr(ctx, "$t名片格式", "玩家自设")
						VarSetValueStr(ctx, "$t名片预览", text)
						ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_自动设置"))
					}
				}
			default:
				ok := false
				ctx.Dice.GameSystemMap.Range(func(key string, value *GameSystemTemplate) bool {
					var t NameTemplateItem
					var exists bool

					// 先检查绝对匹配, 不存在则检查小写匹配
					if t, exists = value.NameTemplate[val]; !exists {
						t, exists = value.NameTemplate[strings.ToLower(val)]
					}

					if !exists {
						return true
					}

					// 增加使用sn设置自定义规则的名片模板时的错误反馈
					text, err := SetPlayerGroupCardByTemplate(ctx, t.Template)
					if errors.Is(err, ErrGroupCardOverlong) {
						handleOverlong(ctx, msg, text)
						ok = true
						return false
					} else if err != nil {
						ReplyToSender(ctx, msg, "命名模版错误或不存在，请使用.sn help查看使用说明")
						ok = true
						return false
					}
					ctx.Player.AutoSetNameTemplate = t.Template
					identityBindNoteNameTemplate(ctx, false)
					VarSetValueStr(ctx, "$t名片格式", val)
					VarSetValueStr(ctx, "$t名片预览", text)
					ReplyToSender(ctx, msg, DiceFormatTmpl(ctx, "日志:名片_自动设置"))
					ok = true
					return false
				})

				if ok {
					return CmdExecuteResult{Matched: true, Solved: true}
				}

				return CmdExecuteResult{Matched: true, Solved: true, ShowHelp: true}
			}
			return CmdExecuteResult{Matched: true, Solved: true}
		},
	}

	self.RegisterExtension(&ExtInfo{
		Name:       "log",
		Version:    "1.0.1",
		Brief:      "跑团辅助扩展，提供日志、染色等功能",
		Author:     "木落",
		AutoActive: true,
		Official:   true,
		OnLoad: func() {
			// 回调签名里没有 Dice，这里留给上面的去重闭包读配置用。
			logExtDice = self
			_ = os.MkdirAll(filepath.Join(self.BaseConfig.DataDir, "log-exports"), 0o755)
		},
		OnMessageSend: func(ctx *MsgContext, msg *Message, flag string) {
			// 记录骰子发言
			if flag == "skip" {
				return
			}
			privateCommandListenCheck()
			if msg.MessageType == "private" && ctx.CommandHideFlag != "" {
				if privateCommandListenHas(ctx.CommandID) {
					session := ctx.Session
					// CommandHideFlag 里存的是**真实群号**（暗骰时记下"这条回复属于哪个群"），
					// 群绑定之后日志状态与记录都在归一后的旧群名下，
					// 所以这里也要归一：
					//   · 用真实群对象取状态 → 绑群时状态已被清空 → On=false → 暗骰内容静默丢失；
					//   · 就算取到，写入也得落同一个群，否则同一份日志被劈成两个 group_id。
					logGroupID := identityBindLogWriteGroupID(ctx, ctx.CommandHideFlag)
					groupInfo, ok := session.ServiceAtNew.Load(logGroupID)
					if !ok {
						ctx.Dice.Logger.Warn("ServiceAtNew ext_log加载groupInfo异常")
						return
					}
					logState := ensureGroupLogState(ctx, groupInfo)
					if !logState.On || logState.Name == "" {
						return
					}
					a := model.LogOneItem{
						Nickname:    ctx.EndPoint.Nickname,
						IMUserID:    UserIDExtract(ctx.EndPoint.UserID),
						UniformID:   ctx.EndPoint.UserID,
						Time:        time.Now().Unix(),
						Message:     msg.Message,
						IsDice:      true,
						CommandID:   ctx.CommandID,
						CommandInfo: ctx.CommandInfo,
						RawMsgID:    msg.RawID,
					}

					LogAppend(ctx, groupInfo.GroupID, logState.ID, logState.Name, &a)
				}
			}

			if IsCurGroupBotOnByID(ctx.Session, ctx.EndPoint, msg.MessageType, msg.GroupID) {
				session := ctx.Session
				// 玩家发言（OnMessageReceived）已经写到归一后的群了，骰子自己的发言
				// 也必须写同一个群，否则日志会裂成「玩家一条、骰子一条」两份文件。
				groupID := identityBindLogWriteGroupID(ctx, msg.GroupID)
				groupInfo, ok := session.ServiceAtNew.Load(groupID)
				if !ok {
					ctx.Dice.Logger.Warn("ServiceAtNew ext_log加载groupInfo异常")
					return
				}
				logState := ensureGroupLogState(ctx, groupInfo)
				if logState.On && logState.Name != "" {
					// <2022-02-15 09:54:14.0> [摸鱼king]: 有的 但我不知道
					if ctx.CommandHideFlag != "" {
						// 记录当前指令和时间
						privateCommandListenSet(ctx.CommandID, time.Now().Unix())
					}

					a := model.LogOneItem{
						Nickname:    ctx.EndPoint.Nickname,
						IMUserID:    UserIDExtract(ctx.EndPoint.UserID),
						UniformID:   ctx.EndPoint.UserID,
						Time:        time.Now().Unix(),
						Message:     msg.Message,
						IsDice:      true,
						CommandID:   ctx.CommandID,
						CommandInfo: ctx.CommandInfo,
						RawMsgID:    msg.RawID,
					}
					LogAppend(ctx, groupInfo.GroupID, logState.ID, logState.Name, &a)
				}
			}
		},
		OnMessageReceived: func(ctx *MsgContext, msg *Message) {
			// 处理日志
			if ctx.Group != nil {
				// 群绑定之后，官方群和旧群共用同一份日志状态与日志记录。
				// 状态挂在归一后的群对象上，记录也写进归一后的群，
				// 这样「一边 .log on，另一边立刻跟着记」。
				groupID := identityBindLogWriteGroupID(ctx, ctx.Group.GroupID)
				logGroup := ctx.Group
				if stateGroup, bound := identityBindReadGroup(ctx); bound && stateGroup != nil {
					logGroup = stateGroup
				}
				logState := ensureGroupLogState(ctx, logGroup)
				if logState.On && logState.Name != "" {
					// 去重：默认按 msg.RawID（与上游一致）；只有在显式调大
					// logMultiBotDedupWindowSec 之后才改为按「群+人+正文」跨连接去重。
					dedupKey := logDedupKey(ctx, msg)
					if !groupMsgInfoCheckOk(dedupKey) {
						return
					}
					groupMsgInfoSet(dedupKey)

					// <2022-02-15 09:54:14.0> [摸鱼king]: 有的 但我不知道
					a := model.LogOneItem{
						Nickname:  ctx.Player.Name,
						IMUserID:  UserIDExtract(ctx.Player.UserID),
						UniformID: ctx.Player.UserID,
						Time:      msg.Time,
						Message:   msg.Message,
						IsDice:    false,
						CommandID: ctx.CommandID,
						RawMsgID:  msg.RawID,
					}

					LogAppend(ctx, groupID, logState.ID, logState.Name, &a)
				}
			}
		},
		OnMessageDeleted: func(ctx *MsgContext, msg *Message) {
			if ctx.Group == nil {
				return
			}
			// 日志状态挂在归一后的群对象上，所以开关判断也必须用那个对象，
			// 否则官方群里删消息时会被当成「没开日志」而漏删。
			logGroup := ctx.Group
			if stateGroup, bound := identityBindReadGroup(ctx); bound && stateGroup != nil {
				logGroup = stateGroup
			}
			if getGroupLogOn(logGroup) {
				// 日志条目是按归一后的群存的，删除也必须用同一个群 ID 才找得到。
				LogDeleteByID(ctx, identityBindLogWriteGroupID(ctx, ctx.Group.GroupID), msg.RawID)
				// ctx.Session.Parent.Logger.Infof("删除日志 %s %s", ctx.Group.GroupId, msg.RawId.(string))
			}
		},
		OnMessageEdit: func(ctx *MsgContext, msg *Message) {
			if ctx.Group == nil {
				return
			}
			logGroup := ctx.Group
			if stateGroup, bound := identityBindReadGroup(ctx); bound && stateGroup != nil {
				logGroup = stateGroup
			}
			if getGroupLogOn(logGroup) {
				LogEditByID(ctx, identityBindLogWriteGroupID(ctx, ctx.Group.GroupID), msg.Message, msg.RawID)
			}
		},
		GetDescText: GetExtensionDesc,
		CmdMap: CmdMapCls{
			"log":  cmdLog,
			"stat": cmdStat,
			"hiy":  cmdStat,
			"ob":   cmdOb,
			"sn":   cmdSn,
		},
	})
}

func getSpecifiedGroupIfMaster(ctx *MsgContext, msg *Message, cmdArgs *CmdArgs) (groupID string, requestForAnotherGroup bool) {
	if data := cmdArgs.GetArgN(2); data != "" {
		if ctx.PrivilegeLevel < 100 {
			ReplyToSender(ctx, msg, "你并非Master，请检查指令输入是否正确")
			return "", true
		}

		var prefix string
		if ctx.EndPoint.Platform == "QQ" {
			prefix = "QQ-Group"
		}
		if !strings.HasPrefix(data, prefix) {
			data = prefix + ":" + data
		}

		// _newGroup := ctx.Session.ServiceAtNew[data]
		// if _newGroup == nil {
		// 	ReplyToSender(ctx, msg, "找不到指定的群组，请输入正确群号。如在非QQ平台取log，群号请写 QQ-Group:12345")
		// 	return nil, true
		// }
		return data, true
	}
	// 对应的组，是否存在第二个参数
	return "", false
}

func FilenameReplace(name string) string {
	re := regexp.MustCompile(`[/:\*\?"<>\|\\]`)
	return re.ReplaceAllString(name, "")
}

func LogAppend(ctx *MsgContext, groupID string, logID uint64, logName string, logItem *model.LogOneItem) bool {
	ok := false
	if logID > 0 {
		ok = service.LogAppendByID(ctx.Dice.DBOperator, logID, groupID, logItem)
	} else if logName != "" {
		ok = service.LogAppend(ctx.Dice.DBOperator, groupID, logName, logItem)
	}
	if ok {
		if size, okCount := service.LogLinesCountGet(ctx.Dice.DBOperator, groupID, logName); okCount {
			// 默认每记录500条发出提示
			if ctx.Dice.Config.LogSizeNoticeEnable {
				if ctx.Dice.Config.LogSizeNoticeCount == 0 {
					ctx.Dice.Config.LogSizeNoticeCount = DefaultConfig.LogSizeNoticeCount
				}
				if size > 0 && int(size)%ctx.Dice.Config.LogSizeNoticeCount == 0 {
					VarSetValueInt64(ctx, "$t条数", size)
					text := DiceFormatTmpl(ctx, "日志:记录_条数提醒")
					// text := fmt.Sprintf("提示: 当前故事的文本已经记录了 %d 条", size)
					ReplyToSenderRaw(ctx, &Message{MessageType: "group", GroupID: groupID}, text, "skip")
				}
			}
		}
	}
	return ok
}

func LogDeleteByID(ctx *MsgContext, groupID string, messageID interface{}) bool {
	err := service.LogMarkDeleteByRawMsgID(ctx.Dice.DBOperator, groupID, messageID)
	if err != nil {
		ctx.Dice.Logger.Error("LogDeleteById:", zap.Error(err))
		return false
	}
	return true
}

// LogEditByID finds the log item under logName with messageID and replace it with content.
// If the log item cannot be found or an error happens, it returns false.
func LogEditByID(ctx *MsgContext, groupID, content string, messageID interface{}) bool {
	err := service.LogEditByRawMsgID(ctx.Dice.DBOperator, groupID, content, messageID)
	if err != nil {
		ctx.Dice.Logger.Error("LogEditByID:", zap.Error(err))
		return false
	}
	return true
}

func GetLogTxt(ctx *MsgContext, groupID string, logName string, fileNamePrefix string) (string, string, error) {
	// 创建临时文件
	tempPattern, notice := storylog.BuildTempPattern(fileNamePrefix)
	tempLog, err := os.CreateTemp("", tempPattern)
	if err != nil {
		return "", notice, errors.New("log导出出现未知错误")
	}
	defer func() {
		_ = tempLog.Close()
		if err != nil {
			_ = os.Remove(tempLog.Name()) //nolint:gosec
		}
	}()

	counter := 0
	currentCursor := paginator.Cursor{} // 初始游标为空

	// 腾讯元宝: 创建带10秒超时的 context
	ctxWithTimeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel() // 确保 context 被正确取消

	// 腾讯元宝: 使用 channel 接收 goroutine 的结果
	resultCh := make(chan error, 1)
	go func() {
		defer close(resultCh) // 确保 channel 被关闭

		for {
			select {
			case <-ctxWithTimeout.Done(): // 检查是否超时或被取消
				resultCh <- errors.New("日志导出超时（10秒限制），请尝试减少数据量或联系管理员")
				return
			default:
				// 获取当前游标对应的数据
				cursorLines, cursor, err := service.LogGetCursorLines(ctx.Dice.DBOperator, groupID, logName, currentCursor)
				if err != nil {
					resultCh <- err
					return
				}

				// 写入当前批次的数据
				for _, line := range cursorLines {
					timeTxt := time.Unix(line.Time, 0).Format("2006-01-02 15:04:05")
					text := fmt.Sprintf("%s(%v) %s\n%s\n\n", line.Nickname, line.IMUserID, timeTxt, line.Message)
					if _, err = tempLog.WriteString(text); err != nil {
						resultCh <- fmt.Errorf("写入日志导出临时文件失败: %w", err)
						return
					}
					counter++
				}
				// ========== 新增：每批写入后强制同步 ==========
				if err := tempLog.Sync(); err != nil { // 确保批次数据落盘
					resultCh <- fmt.Errorf("批次同步失败: %w", err)
					return
				}

				// 如果没有下一页，则成功完成
				if cursor.After == nil {
					resultCh <- nil
					return
				}

				// 更新游标，继续获取下一页
				currentCursor.After = cursor.After
			}
		}
	}()

	// 等待 goroutine 完成或超时
	if err := <-resultCh; err != nil {
		return "", notice, err
	}
	// 2. 确保文件指针回到开头
	if _, err := tempLog.Seek(0, 0); err != nil {
		return "", notice, fmt.Errorf("重置文件指针失败: %w", err)
	}

	// 如果没有任何数据，返回错误
	if counter == 0 {
		return "", notice, errors.New("此log不存在，或条目数为空，名字是否正确？")
	}

	return tempLog.Name(), notice, nil
}

func LogSendToBackend(ctx *MsgContext, groupID string, logName string) (bool, string, string, error) {
	return logSendToBackend(ctx, groupID, logName, false)
}

func logSendToBackend(ctx *MsgContext, groupID string, logName string, skipResolve bool) (bool, string, string, error) {
	dice := ctx.Dice
	dirPath := filepath.Join(dice.BaseConfig.DataDir, "log-exports")

	if !skipResolve {
		resolvedName, err := resolveLogNameForGroup(dice.DBOperator, groupID, logName)
		if err != nil {
			return false, "", "", err
		}
		if resolvedName != "" {
			logName = resolvedName
		}
	}

	var sealBackends []string
	for _, sealBackend := range BackendUrls {
		sealBackends = append(sealBackends, sealBackend+"/dice/api/log")
	}

	uploadCtx := storylog.UploadEnv{
		Dir: dirPath,
		// 原则上说，上传应该都是读的db，但以防万一，还是传Operator
		Db:       dice.DBOperator,
		Log:      dice.Logger,
		Backends: sealBackends,

		LogName:   logName,
		UniformID: ctx.EndPoint.UserID,
		GroupID:   groupID,
	}
	uploadCtx.Version = storylog.StoryVersionV1

	var unofficial bool
	if dice.AdvancedConfig.Enable && dice.AdvancedConfig.StoryLogBackendUrl != "" {
		unofficial = true
		uploadCtx.Backends = []string{dice.AdvancedConfig.StoryLogBackendUrl}
		uploadCtx.Token = dice.AdvancedConfig.StoryLogBackendToken
	}
	// 原则上1.5支持兼容V1的上传接口，只是需要木落改改代码
	if dice.AdvancedConfig.Enable && dice.AdvancedConfig.StoryLogApiVersion != "" {
		switch dice.AdvancedConfig.StoryLogApiVersion {
		case storylog.StoryVersionV1Str:
			uploadCtx.Version = storylog.StoryVersionV1
		case storylog.StoryVersionV105Str:
			uploadCtx.Version = storylog.StoryVersionV105
		default:
			uploadCtx.Version = storylog.StoryVersionV1
		}
	}

	url, notice, err := storylog.Upload(uploadCtx)
	if err != nil {
		return unofficial, "", notice, err
	}
	if len(url) == 0 {
		return unofficial, "", notice, errors.New("上传 log 到服务器失败，未能获取染色器链接")
	}
	return unofficial, url, notice, nil
}

// LogRollBriefByPCV2 根据log生成骰点简报 采用gjson进行解析 拒绝一次性加载所有数据库数据
func LogRollBriefByPCV2(ctx *MsgContext, items []string, showAll bool, name string) string {
	pcInfo := map[string]map[string]int{}
	// 加载同义词
	tmpl := ctx.Group.GetCharTemplate(ctx.Dice)

	getName := func(s string) string {
		re := regexp.MustCompile(`^([^\d\s]+)(\d+)?$`)
		m := re.FindStringSubmatch(s)
		if len(m) > 0 {
			s = m[1]
		}

		return tmpl.GetAlias(s)
	}

	for _, i := range items {
		// 使用gjson进行解析
		info := gjson.Parse(i)

		setupName := func(name string) {
			if _, exists := pcInfo[name]; !exists {
				pcInfo[name] = map[string]int{}
			}
		}

		if !info.Get("rule").Exists() {
			switch info.Get("cmd").String() {
			case "roll":
				if !info.Get("items").IsArray() {
					continue
				}
				nickname := info.Get("pcName").String()
				setupName(nickname)
				pcInfo[nickname]["骰点"] += len(info.Get("items").Array())
			}
			continue
		}
		if info.Get("rule").String() == "coc7" {
			switch info.Get("cmd").String() {
			case "ra":
				ok2 := info.Get("items").IsArray()
				if !ok2 {
					continue
				}
				nickname := info.Get("pcName").String()
				setupName(nickname)

				for _, j := range info.Get("items").Array() {
					rank := j.Get("rank").Float()
					attr := getName(j.Get("expr2").String())
					if rank > 0 {
						key := fmt.Sprintf("%v:%v", attr, "成功")
						pcInfo[nickname][key]++
					} else if rank < 0 {
						key := fmt.Sprintf("%v:%v", attr, "失败")
						pcInfo[nickname][key]++
					}
				}
				continue
			case "sc":
				ok2 := info.Get("items").IsArray()
				if !ok2 {
					continue
				}
				nickname := info.Get("pcName").String()
				setupName(nickname)

				for _, j := range info.Get("items").Array() {
					rank := j.Get("rank").Float()
					if rank > 0 {
						key := fmt.Sprintf("%v:%v", "理智", "成功")
						pcInfo[nickname][key]++
					} else if rank < 0 {
						key := fmt.Sprintf("%v:%v", "理智", "失败")
						pcInfo[nickname][key]++
					}

					// 如果没有旧值，弄一个
					key := "理智:旧值"
					if pcInfo[nickname][key] == 0 {
						pcInfo[nickname][key] = int(j.Get("sanOld").Int())
					}

					key2 := "理智:新值"
					// if pcInfo[nickname][key2] == 0 {
					pcInfo[nickname][key2] = int(j.Get("sanNew").Int())
					// }
				}
				continue
			case "st":
				ok2 := info.Get("items").IsArray()
				if !ok2 {
					continue
				}
				for _, j := range info.Get("items").Array() {
					nickname := info.Get("pcName").String()
					setupName(nickname)

					if j.Get("type").String() == "mod" {
						readNum := func(item gjson.Result, dataKey, key string) {
							if val := item.Get(dataKey); val.Exists() {
								if val.Type == gjson.Number {
									pcInfo[nickname][key] = int(val.Int()) // 自动处理 float/int/string 数字
								} else {
									// 保存的是 DiceScript 的 IntValue
									if dsVal, dsErr := ds.VMValueFromJSON([]byte(val.Raw)); dsErr == nil {
										if dsInt, dsOK := dsVal.ReadInt(); dsOK {
											pcInfo[nickname][key] = int(dsInt)
										}
									}
								}
							}
							// 如果字段不存在或非数字，pcInfo[nickname][key] 不会被修改
						}

						attr := getName(j.Get("attr").String())
						// 如果没有旧值，弄一个
						key := fmt.Sprintf("%v:旧值", attr)
						if pcInfo[nickname][key] == 0 {
							readNum(j, "valOld", key)
						}

						key2 := fmt.Sprintf("%v:新值", attr)
						// if pcInfo[nickname][key2] == 0 {
						readNum(j, "valNew", key2)
						// }
					}
				}
				continue
			}
		}
	}

	if !showAll {
		pcInfo2 := map[string]map[string]int{}
		if pcInfo[name] != nil {
			pcInfo2[name] = pcInfo[name]
		}
		pcInfo = pcInfo2
	}

	var texts strings.Builder
	for k, v := range pcInfo {
		if len(v) == 0 {
			continue
		}
		fmt.Fprintf(&texts, "<%v>当前团内检定情况:\n", k)
		success := map[string]int{}
		failed := map[string]int{}
		var others []string

		oldVal := map[string]int{}
		newVal := map[string]int{}

		for k2, v2 := range v {
			if strings.HasSuffix(k2, ":成功") {
				success[k2] = v2
			} else if strings.HasSuffix(k2, ":失败") {
				failed[k2] = v2
			} else if strings.HasSuffix(k2, ":旧值") {
				oldVal[k2[:len(k2)-len(":旧值")]] = v2
			} else if strings.HasSuffix(k2, ":新值") {
				newVal[k2[:len(k2)-len(":新值")]] = v2
			} else {
				others = append(others, k2)
			}
		}

		// 排序: 一次挑选一个最大的，直到结束
		doSort := func(m map[string]int) []string {
			var ret []string
			for len(m) > 0 {
				val := -1
				theKey := ""
				for k2, v2 := range m {
					if v2 > val {
						theKey = k2
						val = v2
					}
				}
				ret = append(ret, theKey)
				delete(m, theKey)
			}
			return ret
		}
		successList := doSort(success)
		failedList := doSort(failed)

		if len(successList) > 0 {
			var text strings.Builder
			text.WriteString("成功: ")
			for _, j := range successList {
				fmt.Fprintf(&text, "%v%d ", j[:len(j)-len(":成功")], v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(failedList) > 0 {
			var text strings.Builder
			text.WriteString("失败: ")
			for _, j := range failedList {
				fmt.Fprintf(&text, "%v%d ", j[:len(j)-len(":失败")], v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(oldVal) > 0 {
			var text strings.Builder
			for k2, v2 := range oldVal {
				fmt.Fprintf(&text, "%v[%v➯%v] ", k2, v2, newVal[k2])
			}
			texts.WriteString("属性: ")
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(others) > 0 {
			var text strings.Builder
			text.WriteString("其他: ")
			for _, j := range others {
				fmt.Fprintf(&text, "%v%d ", j, v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}
		texts.WriteString("\n")
	}
	return strings.TrimSpace(texts.String())
}

// LogRollBriefByPC 根据log生成骰点简报
func LogRollBriefByPC(ctx *MsgContext, items []*model.LogOneItem, showAll bool, name string) string {
	pcInfo := map[string]map[string]int{}
	// 加载同义词
	tmpl := ctx.Group.GetCharTemplate(ctx.Dice)

	getName := func(s string) string {
		re := regexp.MustCompile(`^([^\d\s]+)(\d+)?$`)
		m := re.FindStringSubmatch(s)
		if len(m) > 0 {
			s = m[1]
		}

		return tmpl.GetAlias(s)
	}

	for _, i := range items {
		if i.CommandInfo != nil { //nolint:nestif
			info, _ := i.CommandInfo.(map[string]interface{})
			// t := time.Unix(i.Time, 0).Format("[04:05]")

			setupName := func(name string) {
				if _, exists := pcInfo[name]; !exists {
					pcInfo[name] = map[string]int{}
				}
			}

			if info["rule"] == nil {
				switch info["cmd"] {
				case "roll":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					nickname := fmt.Sprintf("%v", info["pcName"])
					setupName(nickname)
					pcInfo[nickname]["骰点"] += len(items)
				}
				continue
			}
			if info["rule"] == "coc7" {
				switch info["cmd"] {
				case "ra":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					nickname := fmt.Sprintf("%v", info["pcName"])
					setupName(nickname)

					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						rank := int(j["rank"].(float64))
						attr := getName(fmt.Sprintf("%v", j["expr2"]))
						if rank > 0 {
							key := fmt.Sprintf("%v:%v", attr, "成功")
							pcInfo[nickname][key]++
						} else if rank < 0 {
							key := fmt.Sprintf("%v:%v", attr, "失败")
							pcInfo[nickname][key]++
						}
					}
					continue
				case "sc":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					nickname := fmt.Sprintf("%v", info["pcName"])
					setupName(nickname)

					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						rank := int(j["rank"].(float64))
						if rank > 0 {
							key := fmt.Sprintf("%v:%v", "理智", "成功")
							pcInfo[nickname][key]++
						} else if rank < 0 {
							key := fmt.Sprintf("%v:%v", "理智", "失败")
							pcInfo[nickname][key]++
						}

						// 如果没有旧值，弄一个
						key := "理智:旧值"
						if pcInfo[nickname][key] == 0 {
							pcInfo[nickname][key] = int(j["sanOld"].(float64))
						}

						key2 := "理智:新值"
						// if pcInfo[nickname][key2] == 0 {
						pcInfo[nickname][key2] = int(j["sanNew"].(float64))
						// }
					}
					continue
				case "st":
					items, ok2 := info["items"].([]any)
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]any)
						if !ok2 {
							continue
						}
						nickname := fmt.Sprintf("%v", info["pcName"])
						setupName(nickname)

						if j["type"] == "mod" {
							readNum := func(dataKey, key string) {
								if val, ok := j[dataKey].(float64); ok {
									// 旧版本兼容，float64是因为json unmarshal默认就是这个
									pcInfo[nickname][key] = int(val)
								} else {
									// TODO: 处理的不是很好，这里后续大段代码依赖了值为int的情况，但是现在实际可以为任何类型，只是不常用
									b, _ := json.Marshal(j[dataKey])
									var v ds.VMValue
									if err := v.UnmarshalJSON(b); err == nil {
										if v.TypeId == ds.VMTypeInt {
											i, _ := v.ReadInt()
											pcInfo[nickname][key] = int(i)
										}
									}
								}
							}

							attr := getName(j["attr"].(string))
							// 如果没有旧值，弄一个
							key := fmt.Sprintf("%v:旧值", attr)
							if pcInfo[nickname][key] == 0 {
								readNum("valOld", key)
							}

							key2 := fmt.Sprintf("%v:新值", attr)
							// if pcInfo[nickname][key2] == 0 {
							readNum("valNew", key2)
							// }
						}
					}
					continue
				}
			}
		}
	}

	if !showAll {
		pcInfo2 := map[string]map[string]int{}
		if pcInfo[name] != nil {
			pcInfo2[name] = pcInfo[name]
		}
		pcInfo = pcInfo2
	}

	var texts strings.Builder
	for k, v := range pcInfo {
		if len(v) == 0 {
			continue
		}
		fmt.Fprintf(&texts, "<%v>当前团内检定情况:\n", k)
		success := map[string]int{}
		failed := map[string]int{}
		var others []string

		oldVal := map[string]int{}
		newVal := map[string]int{}

		for k2, v2 := range v {
			if strings.HasSuffix(k2, ":成功") {
				success[k2] = v2
			} else if strings.HasSuffix(k2, ":失败") {
				failed[k2] = v2
			} else if strings.HasSuffix(k2, ":旧值") {
				oldVal[k2[:len(k2)-len(":旧值")]] = v2
			} else if strings.HasSuffix(k2, ":新值") {
				newVal[k2[:len(k2)-len(":新值")]] = v2
			} else {
				others = append(others, k2)
			}
		}

		// 排序: 一次挑选一个最大的，直到结束
		doSort := func(m map[string]int) []string {
			var ret []string
			for len(m) > 0 {
				val := -1
				theKey := ""
				for k2, v2 := range m {
					if v2 > val {
						theKey = k2
						val = v2
					}
				}
				ret = append(ret, theKey)
				delete(m, theKey)
			}
			return ret
		}
		successList := doSort(success)
		failedList := doSort(failed)

		if len(successList) > 0 {
			var text strings.Builder
			text.WriteString("成功: ")
			for _, j := range successList {
				fmt.Fprintf(&text, "%v%d ", j[:len(j)-len(":成功")], v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(failedList) > 0 {
			var text strings.Builder
			text.WriteString("失败: ")
			for _, j := range failedList {
				fmt.Fprintf(&text, "%v%d ", j[:len(j)-len(":失败")], v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(oldVal) > 0 {
			var text strings.Builder
			for k2, v2 := range oldVal {
				fmt.Fprintf(&text, "%v[%v➯%v] ", k2, v2, newVal[k2])
			}
			texts.WriteString("属性: ")
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}

		if len(others) > 0 {
			var text strings.Builder
			text.WriteString("其他: ")
			for _, j := range others {
				fmt.Fprintf(&text, "%v%d ", j, v[j])
			}
			texts.WriteString(strings.TrimSpace(text.String()))
			texts.WriteString("\n")
		}
		texts.WriteString("\n")
	}
	return strings.TrimSpace(texts.String())
}

// LogRollBriefDetail 根据log生成骰点简报
// TODO：新逻辑下它不可用
func LogRollBriefDetail(items []*model.LogOneItem) []string {
	var texts []string
	for _, i := range items {
		if i.CommandInfo != nil { //nolint:nestif
			info, _ := i.CommandInfo.(map[string]interface{})
			t := time.Unix(i.Time, 0).Format("[04:05]")

			if info["rule"] == nil {
				switch info["cmd"] {
				case "roll":
					// [03分20秒] 木落 骰点d100，出目15
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						reasonText := ""
						if j["reason"] != nil {
							reasonText = fmt.Sprintf(" 原因:%v", j["reason"])
						}

						texts = append(texts, fmt.Sprintf("%v %s 骰点%v 出目%v%v", t, info["pcName"], j["expr"], j["result"], reasonText))
					}
				}
				continue
			}
			if info["rule"] == "coc7" {
				switch info["cmd"] {
				case "ra":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						// [18分60秒] 木落 "力量50"检定，出目39/50，成功
						texts = append(texts, fmt.Sprintf("%v %s \"%s\"检定 出目%v/%v，%v", t, info["pcName"], j["expr2"], j["checkVal"], j["attrVal"], SimpleCocSuccessRankToText[int(j["rank"].(float64))]))
					}
					continue
				case "sc":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						// [18分60秒] 木落 理智检定[d100 1 2]，出目15/60，失败。理智39➯38
						texts = append(texts, fmt.Sprintf("%v %s 理智检定%v 出目%v/%v，%v。理智%v➯%v",
							t, info["pcName"], j["exprs"], j["checkVal"], j["sanOld"],
							SimpleCocSuccessRankToText[int(j["rank"].(float64))], j["sanOld"], j["sanNew"]))
					}
					continue
				case "st":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						if j["type"] == "mod" {
							// [18分60秒] 木落 hp变更1d4，39➯38
							texts = append(texts, fmt.Sprintf("%v %s %v变更%v，%v➯%v",
								t, info["pcName"], j["attr"], j["modExpr"], j["valOld"], j["valNew"]))
						}
					}
					continue
				}
			}

			if info["rule"] == "dnd5e" {
				switch info["cmd"] {
				case "rc":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						// [18分60秒] 木落 力量检定，出目24
						texts = append(texts, fmt.Sprintf("%v %s %s检定 出目%v", t, info["pcName"], j["reason"], j["result"]))
					}
					continue
				case "st":
					items, ok2 := info["items"].([]interface{})
					if !ok2 {
						continue
					}
					for _, _j := range items {
						j, ok2 := _j.(map[string]interface{})
						if !ok2 {
							continue
						}

						if j["type"] == "mod" {
							// [18分60秒] 木落 hp变更1d4，39➯38
							texts = append(texts, fmt.Sprintf("%v %s %v变更%v，%v➯%v",
								t, info["pcName"], j["attr"], j["modExpr"], j["valOld"], j["valNew"]))
						}
					}
					continue
				}
			}

			texts = append(texts, fmt.Sprintf("%v\n", i.CommandInfo))
		}
	}
	return texts
}
