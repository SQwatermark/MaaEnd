package autofight

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// 只声明派发需要的字段；养成、伤害和展示数据不参与自动战斗。
type legacyTimelineRoot struct {
	TimeUnit     string  `json:"timeUnit"`
	FPS          float64 `json:"fps"`
	ScenarioList []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Data struct {
			PrepDuration float64 `json:"prepDuration"`
			Tracks       []struct {
				Actions []struct {
					Type      string  `json:"type"`
					StartTime float64 `json:"startTime"`
					Duration  float64 `json:"duration"`
					Name      string  `json:"name"`
					ID        string  `json:"id"`
				} `json:"actions"`
			} `json:"tracks"`
		} `json:"data"`
	} `json:"scenarioList"`
}

type endAxisProject struct {
	FPS       int `json:"fps"`
	Scenarios []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Battle struct {
			SimulationRange struct {
				EndFrame *int `json:"endFrame"`
			} `json:"simulationRange"`
		} `json:"battle"`
		Tracks []*struct {
			SkillCasts []struct {
				ID     string `json:"id"`
				Source struct {
					Kind     string `json:"kind"`
					Action   string `json:"action"`
					SkillKey string `json:"skillKey"`
				} `json:"source"`
				Placement struct {
					StartFrame  *int    `json:"startFrame"`
					AfterCastID *string `json:"afterCastId"`
				} `json:"placement"`
				Presentation struct {
					Disabled bool `json:"disabled"`
				} `json:"presentation"`
			} `json:"skillCasts"`
		} `json:"tracks"`
	} `json:"scenarios"`
}

// decodeEndAxisShareCode 解码 base64url(gzip(JSON))，限制解压后的大小。
func decodeEndAxisShareCode(code string) ([]byte, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(code), "=")
	if trimmed == "" {
		return nil, fmt.Errorf("empty share code")
	}
	compressed, err := base64.RawURLEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("base64url decode: %w", err)
	}
	gr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("gzip reader: %w", err)
	}
	defer gr.Close()
	const maxPlainSize = 5 << 20
	plain, err := io.ReadAll(io.LimitReader(gr, maxPlainSize+1))
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	if len(plain) > maxPlainSize {
		return nil, fmt.Errorf("share code payload too large (>%d bytes)", maxPlainSize)
	}
	return plain, nil
}

// parseEndAxisProject 在输入边界消除格式和帧率差异。schemaVersion 不作为拒绝依据，
// 只校验实际使用的结构和帧率，使新增无关字段的存档仍可读取。
func parseEndAxisProject(data []byte) (*timelineProject, error) {
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, err
	}
	var project *timelineProject
	var err error
	switch header.Kind {
	case "":
		project, err = parseLegacyTimeline(data)
	case "EndaxisProject":
		project, err = parseProjectTimeline(data)
	default:
		return nil, fmt.Errorf("unsupported Endaxis kind %q", header.Kind)
	}
	if err != nil {
		return nil, err
	}
	if len(project.Scenarios) == 0 {
		return nil, fmt.Errorf("missing Endaxis scenarios")
	}
	for i := range project.Scenarios {
		actions := project.Scenarios[i].Actions
		sort.SliceStable(actions, func(i, j int) bool {
			if actions[i].StartTime != actions[j].StartTime {
				return actions[i].StartTime < actions[j].StartTime
			}
			return actions[i].TrackIdx < actions[j].TrackIdx
		})
	}
	return project, nil
}

func parseLegacyTimeline(data []byte) (*timelineProject, error) {
	var raw legacyTimelineRoot
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	project := &timelineProject{}
	// 早期存档使用秒，之后由 timeUnit/fps 标记为帧；准备区长度可由用户调整。
	fps := 1.0
	if raw.TimeUnit == "frame" {
		if raw.FPS <= 0 {
			return nil, fmt.Errorf("invalid legacy fps %v", raw.FPS)
		}
		fps = raw.FPS
	} else if raw.TimeUnit != "" {
		return nil, fmt.Errorf("unsupported legacy time unit %q", raw.TimeUnit)
	}
	toDuration := func(value float64) time.Duration {
		return time.Duration(value / fps * float64(time.Second))
	}
	for _, source := range raw.ScenarioList {
		sc := timelineScenario{ID: source.ID, Name: source.Name}
		frameBase := source.Data.PrepDuration
		if frameBase < 0 {
			return nil, fmt.Errorf("scenario %q has negative preparation duration", source.ID)
		}
		endFrame := frameBase
		for trackIdx, track := range source.Data.Tracks {
			for _, action := range track.Actions {
				// 保留旧版语义：结束时间包括不由 MaaEnd 派发的普攻等动作的持续时间。
				endFrame = max(endFrame, action.StartTime+action.Duration)
				actionType := action.Type
				if actionType == "battleSkill" {
					actionType = "skill"
				}
				if actionType != "skill" && actionType != "ultimate" {
					continue
				}
				sc.Actions = append(sc.Actions, EndAxisAction{
					Type: actionType, TrackIdx: trackIdx,
					StartTime: toDuration(action.StartTime - frameBase),
					Name:      action.Name, ID: action.ID,
				})
			}
		}
		sc.EndTime = toDuration(endFrame - frameBase)
		project.Scenarios = append(project.Scenarios, sc)
	}
	return project, nil
}

func parseProjectTimeline(data []byte) (*timelineProject, error) {
	var raw endAxisProject
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw.FPS != 30 {
		return nil, fmt.Errorf("unsupported Endaxis fps %d", raw.FPS)
	}
	project := &timelineProject{}
	for _, source := range raw.Scenarios {
		sc := timelineScenario{ID: source.ID, Name: source.Name}
		endFrame := source.Battle.SimulationRange.EndFrame
		if endFrame != nil && *endFrame < 0 {
			return nil, fmt.Errorf("scenario %q has negative simulation end", source.ID)
		}
		lastStart := time.Duration(0)
		for trackIdx, track := range source.Tracks {
			// 空轨道不能被压缩，否则派发到游戏中的角色位置会改变。
			if track == nil {
				continue
			}
			for castIndex, cast := range track.SkillCasts {
				if cast.Presentation.Disabled || cast.Placement.AfterCastID != nil || cast.Placement.StartFrame == nil {
					continue
				}
				start := time.Duration(*cast.Placement.StartFrame) * time.Second / 30
				// 没有终止线时，以所有启用且有绝对起点的动作计算尾部等待，
				// 包括不派发的普攻。连续组后续段没有独立起点，暂不推算。
				lastStart = max(lastStart, start)
				if cast.Source.Kind != "operatorSkill" {
					continue
				}
				actionType := cast.Source.Action
				if actionType == "" {
					return nil, fmt.Errorf("scenario %q (id %q), track %d, skill cast %d (id %q, skill %q): missing source.action", source.Name, source.ID, trackIdx+1, castIndex+1, cast.ID, cast.Source.SkillKey)
				}
				switch actionType {
				case "battleSkill":
					actionType = "skill"
				case "ultimate":
				default:
					continue
				}
				if endFrame != nil && *cast.Placement.StartFrame > *endFrame {
					continue
				}
				sc.Actions = append(sc.Actions, EndAxisAction{
					Type: actionType, TrackIdx: trackIdx, StartTime: start,
					Name: cast.Source.SkillKey, ID: cast.ID,
				})
			}
		}
		sc.EndTime = lastStart + 2*time.Second
		if endFrame != nil {
			sc.EndTime = time.Duration(*endFrame) * time.Second / 30
		}
		project.Scenarios = append(project.Scenarios, sc)
	}
	return project, nil
}
