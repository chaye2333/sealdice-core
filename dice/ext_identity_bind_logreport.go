package dice

import (
	"fmt"
	"sort"
	"strings"

	"sealdice-core/dice/service"
	"sealdice-core/model"
	"sealdice-core/utils/constant"
)

// 本文件负责「群绑定之后日志到底接没接上」的诊断与报错。
//
// 背景：群绑定的本质是**把日志读写改道到旧群**，不做任何数据搬移。
// 于是最常见的两个用户困惑是：
//  1. 绑定成功了，但 .log list 还是空的 —— 因为旧群名下根本没有日志；
//  2. 明明旧群有日志，列表却是空的 —— 因为旧群号填错、或绑定的官方群 ID 变了。
//
// 这两种情况以前都是**静默**的：绑定回执只说「双方共用同一份日志」，
// 后台日志里也一个字都没有，用户和骰主都无从判断问题出在哪。
// 这里把「绑定涉及哪些日志」写进骰主后台日志。
//
// 展示边界（重要）：
//   - 发起 / 验证阶段**绝不能**把旧群日志名/条数发到 QQ——那时还没证明群归属；
//   - 绑定成功之后可以在回执/通知里列出「迁移了哪些日志」，方便用户确认；
//   - 完整清单无论如何都要写进骰主后台日志。

const (
	// identityBindLogPreviewLimit 回执 / 日志里最多列几个日志名，避免刷屏。
	identityBindLogPreviewLimit = 6

	// identityBindLogCandidateLimit 旧群查不到日志时，最多提示几个候选群 ID。
	identityBindLogCandidateLimit = 5
)

// ---------- 日志输出的兜底包装 ----------
// 名字必须带 f 后缀：这三个函数收 format+args，goprintffuncname 会强制要求。

// identityBindInfof 写一条普通日志。d / d.Logger 可能为 nil（极简实例、部分测试）。
func identityBindInfof(d *Dice, format string, args ...any) {
	if d != nil && d.Logger != nil {
		d.Logger.Infof(format, args...)
	}
}

// identityBindWarnf 写一条警告日志。用于「绑定成立，但结果可疑」这类情况。
func identityBindWarnf(d *Dice, format string, args ...any) {
	if d != nil && d.Logger != nil {
		d.Logger.Warnf(format, args...)
	}
}

// identityBindErrorf 写一条错误日志。用于数据库不可用、落盘失败这类真正的故障。
func identityBindErrorf(d *Dice, format string, args ...any) {
	if d != nil && d.Logger != nil {
		d.Logger.Errorf(format, args...)
	}
}

// identityBindGroupLabel 给日志用的「群ID(群名)」标签，群名取不到就只显示 ID。
func identityBindGroupLabel(ctx *MsgContext, groupID string) string {
	if strings.TrimSpace(groupID) == "" {
		return "<未指定>"
	}
	if ctx != nil && ctx.Session != nil {
		if group, ok := ctx.Session.ServiceAtNew.Load(groupID); ok && group != nil {
			if name := strings.TrimSpace(group.GroupName); name != "" {
				return fmt.Sprintf("%s(%s)", groupID, name)
			}
		}
	}
	return groupID
}

// ---------- 日志清单 ----------

// identityBindListGroupLogs 取某个群名下的日志名列表。
// 返回的 error 要原样报给用户，不能吞掉——「查不到」和「查询失败」是两回事。
func identityBindListGroupLogs(d *Dice, groupID string) ([]string, error) {
	if d == nil || d.DBOperator == nil || strings.TrimSpace(groupID) == "" {
		return nil, nil
	}
	return service.LogGetList(d.DBOperator, groupID)
}

// identityBindLogGroupNames 把日志名列表渲染成 `3份（a 12条 / b 0条）`。
// 条数查不到时只显示名字，绝不因为一条统计失败就把整份清单吞掉。
func identityBindLogGroupNames(d *Dice, groupID string, names []string) string {
	if len(names) == 0 {
		return "0 份"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d 份（", len(names))
	limit := len(names)
	if limit > identityBindLogPreviewLimit {
		limit = identityBindLogPreviewLimit
	}
	for i := range limit {
		if i > 0 {
			b.WriteString(" / ")
		}
		b.WriteString(formatLogNameForDisplay(names[i]))
		if d != nil && d.DBOperator != nil {
			if lines, ok := service.LogLinesCountGet(d.DBOperator, groupID, names[i]); ok {
				fmt.Fprintf(&b, " %d条", lines)
			}
		}
	}
	if len(names) > limit {
		fmt.Fprintf(&b, " 等共 %d 份", len(names))
	}
	b.WriteString("）")
	return b.String()
}

// ---------- 绑定后的「日志接续」体检 ----------

// identityBindLogShareSummary 一次群绑定的日志接续结果。
type identityBindLogShareSummary struct {
	NewGroupID string // 官方群（发起绑定、今后写入的那一侧的真实群）
	OldGroupID string // 旧群（日志的规范归属方）
	OldNames   []string
	OldErr     error
	NewNames   []string
	NewErr     error
}

// identityBindInspectLogShare 绑定成立后立刻体检一次：旧群有哪些日志、官方群原来有哪些。
//
// 注意时机：这里读的是 logs 表（数据本身），与清理 GroupInfo 上的运行时日志状态无关，
// 所以放在清理前后都可以。
func identityBindInspectLogShare(d *Dice, newGroupID, oldGroupID string) *identityBindLogShareSummary {
	summary := &identityBindLogShareSummary{NewGroupID: newGroupID, OldGroupID: oldGroupID}
	if d == nil {
		return summary
	}
	if strings.TrimSpace(oldGroupID) != "" {
		summary.OldNames, summary.OldErr = identityBindListGroupLogs(d, oldGroupID)
	}
	if strings.TrimSpace(newGroupID) != "" {
		summary.NewNames, summary.NewErr = identityBindListGroupLogs(d, newGroupID)
	}
	return summary
}

// HasProblem 是否存在「接不上日志」或「查不了日志」的情况，供调用方选 Warn / Info。
func (s *identityBindLogShareSummary) HasProblem() bool {
	if s == nil {
		return true
	}
	return s.OldErr != nil || s.NewErr != nil || len(s.OldNames) == 0
}

// HasError 是否出现真正需要骰主介入的故障（查询本身失败）。
func (s *identityBindLogShareSummary) HasError() bool {
	if s == nil {
		return true
	}
	return s.OldErr != nil || s.NewErr != nil
}

// LogText 一行式摘要，写进骰主后台日志（所以这里不写换行）。
func (s *identityBindLogShareSummary) LogText(d *Dice) string {
	if s == nil {
		return "没有拿到日志清单"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "官方群=%s 旧群=%s", s.NewGroupID, s.OldGroupID)
	switch {
	case s.OldErr != nil:
		fmt.Fprintf(&b, " 读取旧群日志失败=%v", s.OldErr)
	case len(s.OldNames) == 0:
		b.WriteString(" 旧群日志=0份(绑定后 .log list 仍为空)")
	default:
		fmt.Fprintf(&b, " 旧群日志=%s", identityBindLogGroupNames(d, s.OldGroupID, s.OldNames))
	}
	switch {
	case s.NewErr != nil:
		fmt.Fprintf(&b, " 读取官方群原有日志失败=%v", s.NewErr)
	case len(s.NewNames) > 0:
		fmt.Fprintf(&b, " 官方群原有日志=%s(绑定期内隐藏，未删除)",
			identityBindLogGroupNames(d, s.NewGroupID, s.NewNames))
	}
	// 旧群没有日志时，把「号码相同、群 ID 不同」的候选群也写进后台日志。
	// 这类信息只在后台出现：发到 QQ 群里等于把别群的日志名/条数泄露给群成员。
	if s.OldErr == nil && len(s.OldNames) == 0 {
		if candidates := identityBindLogCandidates(d, s.OldGroupID); len(candidates) > 0 {
			fmt.Fprintf(&b, " 候选群={%s}", strings.Join(candidates, "；"))
		}
	}
	return b.String()
}

// ReportText 生成「成功绑定后」给用户看的日志迁移结果。
//
// 只有**绑定已经成立之后**才允许调用：这时官方群与旧群已经共用同一份日志，
// 列出日志名/条数不会造成额外泄露。发起/验证阶段绝不能调用（那时还没证明群归属）。
func (s *identityBindLogShareSummary) ReportText(d *Dice) string {
	if s == nil {
		return ""
	}
	lines := []string{"【日志迁移结果】"}
	switch {
	case s.OldErr != nil:
		lines = append(lines,
			fmt.Sprintf("⚠️ 读取旧群 %s 的日志失败: %v", s.OldGroupID, s.OldErr),
			"   这次绑定本身已生效，但日志是否接上无法确认，请让骰主查看后台日志。")
	case len(s.OldNames) == 0:
		lines = append(lines,
			fmt.Sprintf("⚠️ 旧群 %s 名下没有查到任何日志。", s.OldGroupID),
			"   这次绑定只对「以后新写的日志」生效，历史日志接不过来，.log list 依然会是空的。",
			fmt.Sprintf("   请确认旧群号是否填对（本次填的是 %s）。", identityBindBareNumber(s.OldGroupID)))
		if candidates := identityBindLogCandidates(d, s.OldGroupID); len(candidates) > 0 {
			lines = append(lines, "   本实例里这些群 ID 名下也有日志，可能与旧群号对不上：")
			for _, c := range candidates {
				lines = append(lines, "     · "+c)
			}
		}
	default:
		lines = append(lines,
			fmt.Sprintf("旧群 %s: %s", s.OldGroupID,
				identityBindLogGroupNames(d, s.OldGroupID, s.OldNames)))
	}
	switch {
	case s.NewErr != nil:
		lines = append(lines, fmt.Sprintf("⚠️ 读取官方群 %s 原有日志失败: %v", s.NewGroupID, s.NewErr))
	case len(s.NewNames) > 0:
		lines = append(lines,
			fmt.Sprintf("官方群 %s: 原有 %s", s.NewGroupID,
				identityBindLogGroupNames(d, s.NewGroupID, s.NewNames)),
			"   这些日志不会删除，但绑定期内在列表里看不到；.group unbind 后恢复。")
	}
	return strings.Join(lines, "\n")
}

// identityBindLogShareReportForSuccess 生成「成功绑定之后」回执里的日志清单。
//
// 只允许在绑定已经成立之后调用；发起/验证阶段调用等于提前泄露旧群信息。
func identityBindLogShareReportForSuccess(d *Dice, c *identityBindCodeChallenge) string {
	if c == nil || c.Action != identityBindActionGroup {
		return ""
	}
	return identityBindInspectLogShare(d, c.New.GroupID, c.Old.GroupID).ReportText(d)
}

// identityBindReportLogShare 把体检结果写进骰主后台日志。
//
// 有查询故障用 Error（需要骰主介入），只是"接不上日志"用 Warn，
// 一切正常用 Info。调用方**不要**把这份内容回显到 QQ：
// 日志名与条数是旧群的敏感信息，群消息/私聊都不该带。
// 集中在这里写日志，免得「验证码确认 / 骰主手动确认 / approve」三条路径各写一遍。
func identityBindReportLogShare(d *Dice, prefix string, summary *identityBindLogShareSummary) {
	switch {
	case summary.HasError():
		identityBindErrorf(d, "%s 日志接续异常: %s", prefix, summary.LogText(d))
	case summary.HasProblem():
		identityBindWarnf(d, "%s 日志接续提醒: %s", prefix, summary.LogText(d))
	default:
		identityBindInfof(d, "%s 日志接续: %s", prefix, summary.LogText(d))
	}
}

// identityBindLogCandidates 旧群名下查不到日志时，找找本实例里「号码相同、但群 ID 不同」的群。
//
// 典型场景：用户把旧群号填成了别的群、或数据其实是另一个连接方式下的群号（前缀不同）。
// 把候选列出来，比一句「旧群没有日志」有用得多。
func identityBindLogCandidates(d *Dice, groupID string) []string {
	if d == nil || d.DBOperator == nil {
		return nil
	}
	number := identityBindBareNumber(groupID)
	// 取不出纯数字（官方群 ID 之类）时没有可比对的号码，直接跳过。
	if number == "" || number == groupID || !isAllDigits(number) {
		return nil
	}
	db := d.DBOperator.GetLogDB(constant.READ)
	if db == nil {
		return nil
	}
	var ids []string
	err := db.Model(&model.LogInfo{}).
		Where("group_id <> ? AND (group_id = ? OR group_id LIKE ?)", groupID, number, "%:"+number).
		Distinct().
		Pluck("group_id", &ids).Error
	if err != nil {
		identityBindWarnf(d, "群绑定日志排查失败: 旧群=%s err=%v", groupID, err)
		return nil
	}
	sort.Strings(ids)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if len(out) >= identityBindLogCandidateLimit {
			break
		}
		names, listErr := identityBindListGroupLogs(d, id)
		if listErr != nil || len(names) == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s", id, identityBindLogGroupNames(d, id, names)))
	}
	return out
}

// ---------- .log list 的绑定提示 ----------

// identityBindLogListBindingHint 在 .log list 列表为空时补一句「到底读的是哪个群」。
//
// 为什么需要：绑群之后列表改读旧群，但旧群没日志时会**静默回退**到本群。
// 用户只看到「暂无记录」，完全无法判断是旧群没日志、还是绑定没生效。
func identityBindLogListBindingHint(d *Dice, groupID string) string {
	if d == nil || strings.TrimSpace(groupID) == "" {
		return ""
	}
	record, ok := identityBindStoreOf(d).find(d, groupID)
	if !ok || record.Action != identityBindActionGroup {
		return ""
	}
	if groupID == record.Old.GroupID {
		return fmt.Sprintf(
			"（%s 是绑定关系里的旧群，本群读的就是它；这里为空说明旧群名下确实没有日志。\n"+
				"  想让日志出现，请先在旧群执行 .log new，或核对 .group status 里的旧群号。）", groupID)
	}
	return fmt.Sprintf(
		"（本群已绑定旧群 %s，但旧群名下没有查到日志，所以这里列出的是本群自己的日志。\n"+
			"  请用 .group status 核对旧群号是否正确，或让骰主查看后台日志。）", record.Old.GroupID)
}
