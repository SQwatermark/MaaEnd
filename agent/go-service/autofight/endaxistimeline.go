package autofight

import (
	"slices"
	"time"

	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/i18n"
	"github.com/MaaXYZ/MaaEnd/agent/go-service/pkg/maafocus"
	"github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

// EndAxisAction 是从时间轴中提取出的一个待派发的 ultimate/skill 动作。
type EndAxisAction struct {
	Type      string        // "ultimate" | "skill"
	TrackIdx  int           // 队伍位置，从 0 开始；空轨道仍占一个位置。
	StartTime time.Duration // 相对战斗开始的时间，已消除存档帧率和准备阶段偏移。
	Name      string
	ID        string
}

type timelineScenario struct {
	ID      string
	Name    string
	Actions []EndAxisAction
	EndTime time.Duration
}

type timelineProject struct {
	Scenarios []timelineScenario
}

// EndAxisTimeline 解析 Endaxis 时间轴数据码，并按时间轴派发 ultimate/skill 动作。
//
// 输入格式为 Endaxis（www.end-axis.com）网站"复制数据码"按钮生成的字符串：
// 同时支持旧版 scenarioList 与新版 EndaxisProject。内层为项目数据，外层经过 gzip 压缩并使用 URL-safe
// base64（'+'→'-'、'/'→'_'、去掉尾随 '='）编码。
//
// 用法：
//
//	t := NewEndAxisTimeline()
//	t.SetTimelineCode(code)
//	if t.SelectScenario(ctx, characterCount, comboFull, endSkillFull, energy, skipComboCooldown) {
//	    for !t.ActionFinish() {
//	        if a := t.FrontAction(); a != nil {
//	            // ... 在外部执行该动作 ...
//	            t.PopFrontAction()
//	        }
//	    }
//	}
//
// 时序：SelectScenario 成功后内部计时即开始；当 FrontAction 返回一个动作时，
// 计时被自动暂停，直到 PopFrontAction 调用后再恢复。
type EndAxisTimeline struct {
	root        *timelineProject
	selectedID  string
	queue       []EndAxisAction
	energyLevel int

	started        bool          // SelectScenario 是否成功启动了时间轴
	paused         bool          // 当前是否处于暂停（等待 PopFrontAction）
	startReal      time.Time     // 时间轴起点的真实时间（SelectScenario 成功的瞬间）
	pausedAt       time.Time     // 当前这一段暂停的起始真实时间，仅在 paused=true 时有意义
	pausedDuration time.Duration // 截至上次 resume，已累计的暂停总时长
	endTime        time.Duration // 解析层确定的方案结束时间
}

// NewEndAxisTimeline 返回一个空的时间轴对象，使用前需先调用 SetTimelineCode。
func NewEndAxisTimeline() *EndAxisTimeline {
	return &EndAxisTimeline{}
}

// SetTimelineCode 解析传入的 Endaxis 数据码（base64url(gzip(JSON))）。
// 成功返回 true，失败返回 false。失败时会清空已有的时间轴数据。
func (t *EndAxisTimeline) SetTimelineCode(code string) bool {
	jsonBytes, err := decodeEndAxisShareCode(code)
	if err != nil {
		log.Error().
			Err(err).
			Str("component", "EndAxisTimeline").
			Str("step", "SetTimelineCode").
			Msg("failed to decode timeline share code")
		t.root = nil
		t.reset()
		return false
	}

	root, err := parseEndAxisProject(jsonBytes)
	if err != nil {
		log.Error().
			Err(err).
			Str("component", "EndAxisTimeline").
			Str("step", "SetTimelineCode").
			Msg("failed to parse timeline json")
		t.root = nil
		t.reset()
		return false
	}

	t.root = root
	t.reset()
	log.Info().
		Str("component", "EndAxisTimeline").
		Int("scenarioCount", len(root.Scenarios)).
		Msg("timeline share code loaded")
	return true
}

// SelectScenario 根据当前队伍状态挑选一个匹配的 scenario，并启动时间轴。
//
// 参数：
//   - characterCount：当前队伍的角色数量（1..4）；track 0..characterCount-1 对应队伍里
//     的 1..characterCount 号角色；
//   - characterComboFull：连携已就绪的角色编号列表（1..characterCount 中的子集），
//     例如 [1, 3] 表示 1 号、3 号角色连携满；
//   - endSkillFull：终结技已充能完毕的角色编号列表（1..characterCount 中的子集）；
//   - energyLevel：当前能量条等级，目前仅作为状态保留，未参与匹配；
//   - skipComboCooldown：为 true 时跳过全员连携就绪检测，轴完立刻尝试匹配下一 scenario。
//
// 匹配规则：
//  1. 若 skipComboCooldown 为 false，且 1..characterCount 任一角色不在 characterComboFull 中
//     （即有人连携没满），直接返回 false，不进入 scenario 匹配，并通过 maafocus.PrintThrottle（3s）
//     输出"等待连携技冷却完成"的提示；
//  2. 对每个 scenario，逐个 track i ∈ [0, characterCount) 检查：若该 track 含 type==ultimate
//     的 action，则对应角色编号 i+1 必须在 endSkillFull 列表中；任一项不满足则跳过该
//     scenario，并通过 maafocus.PrintThrottle（3s）输出"终结技未充能完毕"的提示；
//  3. scenario 内若没有可派发的 ultimate / skill 动作，
//     也跳过该 scenario，并通过 maafocus.PrintThrottle（3s）输出"没有战技或终结技"的提示；
//  4. 所有 scenario 都不满足时返回 false。
//
// 选中 scenario 时通过 maafocus.Print 输出多语言提示；跳过提示限频，ctx 为 nil 时仅记录日志。
func (t *EndAxisTimeline) SelectScenario(ctx *maa.Context, characterCount int, characterComboFull, endSkillFull []int, energyLevel int, skipComboCooldown bool) bool {
	t.reset()

	if t.root == nil {
		return false
	}

	if !skipComboCooldown {
		for op := 1; op <= characterCount; op++ {
			if !slices.Contains(characterComboFull, op) {
				log.Debug().
					Str("component", "EndAxisTimeline").
					Str("step", "SelectScenario").
					Int("waitingOperator", op).
					Msg("combo not ready for all operators")
				maafocus.PrintThrottle(ctx, 3*time.Second, i18n.T("autofight.endaxis.waiting_combo_cooldown"))
				return false
			}
		}
	}

	for i := range t.root.Scenarios {
		sc := &t.root.Scenarios[i]
		if !scenarioMatchesEndSkill(sc, endSkillFull, characterCount) {
			log.Debug().
				Str("component", "EndAxisTimeline").
				Str("step", "SelectScenario").
				Str("scenarioId", sc.ID).
				Str("scenarioName", sc.Name).
				Msg("scenario skipped: ultimate gauge not full")
			maafocus.PrintThrottle(ctx, 3*time.Second, i18n.T("autofight.endaxis.scenario_skipped_endskill", sc.Name))
			continue
		}

		actions := sc.Actions
		if len(actions) == 0 {
			log.Debug().
				Str("component", "EndAxisTimeline").
				Str("step", "SelectScenario").
				Str("scenarioId", sc.ID).
				Str("scenarioName", sc.Name).
				Msg("scenario skipped: no skill/ultimate actions")
			maafocus.PrintThrottle(ctx, 3*time.Second, i18n.T("autofight.endaxis.scenario_skipped_no_action", sc.Name))
			continue
		}

		t.selectedID = sc.ID
		t.queue = slices.Clone(actions)
		t.energyLevel = energyLevel
		t.started = true
		t.paused = false
		t.startReal = time.Now()
		t.pausedDuration = 0
		t.endTime = sc.EndTime

		log.Info().
			Str("component", "EndAxisTimeline").
			Str("step", "SelectScenario").
			Str("scenarioId", sc.ID).
			Str("scenarioName", sc.Name).
			Int("actionCount", len(t.queue)).
			Dur("endTime", t.endTime).
			Int("energyLevel", energyLevel).
			Msg("scenario selected")
		maafocus.Print(ctx, i18n.T("autofight.endaxis.scenario_selected", sc.Name))
		return true
	}

	log.Debug().
		Str("component", "EndAxisTimeline").
		Str("step", "SelectScenario").
		Int("scenarioCount", len(t.root.Scenarios)).
		Msg("no matching scenario")
	maafocus.PrintThrottle(ctx, 3*time.Second, i18n.T("autofight.endaxis.no_matching_scenario"))
	return false
}

// FrontAction 返回当前时刻应触发的队首 ultimate/skill 动作。
// 命中时会自动暂停内部计时，直到 PopFrontAction 调用后再恢复。
// 未到时间或队列为空时返回 nil。
func (t *EndAxisTimeline) FrontAction() *EndAxisAction {
	if !t.started || len(t.queue) == 0 {
		return nil
	}
	if t.queue[0].StartTime > t.currentTime() {
		return nil
	}

	t.pause()
	head := t.queue[0]
	return &head
}

// PopFrontAction 删除当前队首动作并恢复计时。队列为空或时间轴未启动时为空操作。
func (t *EndAxisTimeline) PopFrontAction() {
	if !t.started || len(t.queue) == 0 {
		return
	}

	popped := t.queue[0]
	t.queue = t.queue[1:]
	t.resume()

	log.Debug().
		Str("component", "EndAxisTimeline").
		Str("step", "PopFrontAction").
		Str("type", popped.Type).
		Int("trackIdx", popped.TrackIdx).
		Dur("startTime", popped.StartTime).
		Int("remaining", len(t.queue)).
		Msg("action popped")
}

// ActionFinish 返回当前 scenario 的时间轴是否已经结束。
// 同时满足两个条件才视为结束：
//  1. 派发队列已空（所有 ultimate/skill 都已 Pop）；
//  2. 当前逻辑时间已到解析层确定的方案结束时间。
//
// 这样可以避免最后一个 ultimate/skill Pop 完后立刻进入下一次 SelectScenario，
// 留出 scenario 末尾普攻/位移等动作所占用的时间窗口。
func (t *EndAxisTimeline) ActionFinish() bool {
	if !t.started {
		return true
	}
	if len(t.queue) > 0 {
		return false
	}
	return t.currentTime() >= t.endTime
}

// reset 清空运行时状态，但保留已解析的方案。
func (t *EndAxisTimeline) reset() {
	t.selectedID = ""
	t.queue = nil
	t.energyLevel = 0
	t.started = false
	t.paused = false
	t.startReal = time.Time{}
	t.pausedAt = time.Time{}
	t.pausedDuration = 0
	t.endTime = 0
}

// currentTime 返回扣除等待派发暂停时间后的逻辑时间。
func (t *EndAxisTimeline) currentTime() time.Duration {
	if !t.started {
		return 0
	}
	now := time.Now()
	if t.paused {
		now = t.pausedAt
	}
	return now.Sub(t.startReal) - t.pausedDuration
}

func (t *EndAxisTimeline) pause() {
	if t.paused {
		return
	}
	t.pausedAt = time.Now()
	t.paused = true
}

func (t *EndAxisTimeline) resume() {
	if !t.paused {
		return
	}
	t.pausedDuration += time.Since(t.pausedAt)
	t.paused = false
}

// scenarioMatchesEndSkill 检查 scenario 的 track 与终结技就绪情况的对应关系：
// 只要 track i（i ∈ [0, characterCount)）内含 type==ultimate 的 action，
// 对应的角色编号 i+1 就必须出现在 endSkillFull 列表中。
// 超出 characterCount 的 track（队伍里没有对应角色）一律忽略。
func scenarioMatchesEndSkill(sc *timelineScenario, endSkillFull []int, characterCount int) bool {
	for _, action := range sc.Actions {
		if action.TrackIdx < characterCount && action.Type == "ultimate" && !slices.Contains(endSkillFull, action.TrackIdx+1) {
			return false
		}
	}
	return true
}
