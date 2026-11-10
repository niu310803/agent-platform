package voice

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agent-platform/internal/runtime/controlscope"
	rt "agent-platform/internal/runtime/types"
)

// inputRunActive rechecks Runtime facts without adopting another caller's Run.
// A missing active Run is not release proof: the blocking owned call must also
// return before this session can submit a replacement Query.
func (s *session) inputRunActive(run *ownedRun) (bool, error) {
	active, found, err := s.service.deps.Runs.ActiveRunForChat(s.chatID)
	if err != nil {
		return false, errors.New("无法核对当前任务")
	}
	if !found {
		return false, nil
	}
	if run != nil && run.ref.Load() == nil {
		// Registration can precede OnRunStarted. Keep the frozen call, but do
		// not adopt this lookup's identity or send any command until the hook.
		return false, nil
	}
	if run == nil || run.ref.Load().RunID != active.RunID {
		return false, errors.New("当前聊天已有其他任务，请查看聊天界面")
	}
	status, err := s.service.deps.Runtime.GetRunStatus(active.RunID)
	if err != nil || status.ChatID != s.chatID || status.AgentKey != s.agentKey {
		return false, errors.New("无法核对当前任务")
	}
	if status.AwaitingCount > 0 || status.Awaiting != nil {
		return false, errors.New("请在聊天界面处理当前等待项")
	}
	return true, nil
}

func (s *session) operationID(t *turn) string {
	return fmt.Sprintf("voice:%s:%d", s.id, t.number)
}

func (s *session) routeInput(t *turn) error {
	s.clearCompleted() // A nonempty input supersedes the unplayed old result.
	summary, err := s.service.deps.Chats.Summary(s.chatID)
	if err != nil {
		return s.failTurn(t, "run_failed", "无法核对聊天状态")
	}
	if summary == nil && s.chatCreated {
		return s.failTurn(t, "chat_changed", "聊天已归档或删除，请重新连接语音")
	}
	if summary != nil && summary.PendingAwaiting != nil {
		return s.failTurn(t, "input_changed", "当前已有等待项，请查看聊天界面")
	}
	if s.running != nil && s.running != t.inputRun {
		return s.failTurn(t, "input_changed", "当前任务已变化，请查看聊天界面")
	}
	active, err := s.inputRunActive(t.inputRun)
	if err != nil {
		return s.failTurn(t, "input_changed", err.Error())
	}
	if s.running == nil {
		if active {
			return s.failTurn(t, "input_changed", "任务状态已变化，请查看聊天界面")
		}
		return s.submit(t) // Runtime still owns final Query admission.
	}
	if !active || t.inputRun.cancelled.Load() || t.steered {
		return s.waitForRun(t)
	}
	ref := *t.inputRun.ref.Load()
	t.phase, t.steered = "steering", true
	id, text := s.operationID(t), t.text
	s.service.start(func() {
		ctx, cancel := context.WithTimeout(s.service.ctx, writeTimeout)
		defer cancel()
		ctx = controlscope.WithContext(ctx, controlscope.Scope{Transport: "voice", Subject: ref.Caller.Subject})
		result, err := s.service.deps.Runtime.Steer(ctx, rt.SteerCommand{RunRef: ref, RequestID: id, SteerID: id, Message: text})
		stage := "steerRejected"
		switch {
		case err != nil:
			stage = "steerUnknown"
		case result.Accepted:
			stage = "steerAccepted"
		case result.Status == "unmatched":
			stage = "steerUnmatched"
		}
		s.timing(stage, time.Now(), map[string]any{"turn": t.number, "steerId": id, "runId": ref.RunID})
		s.post(s.ctx, sessionEvent{kind: "steerResult", turn: t, run: t.inputRun, steer: result, err: err})
	})
	return nil
}

// One final transcript can wait for its own Run's startup/release. No queue and
// no retry of a Steer whose outcome is unknown.
func (s *session) waitForRun(t *turn) error {
	t.phase = "waiting"
	if s.pending != t {
		s.clearPending()
		s.pending = t
		s.waitTimer = time.NewTimer(finishTimeout)
		s.waited = s.waitTimer.C
	}
	return nil
}

func (s *session) finishSteer(t *turn, result rt.SteerResult, err error) error {
	if t.phase != "steering" {
		return nil
	}
	if err != nil {
		s.clearCompleted()
		return s.failTurn(t, "steer_outcome_unknown", "补充结果未确认，请查看聊天界面；本句话不会自动重发")
	}
	if result.Accepted {
		finishUsage, usageErr := s.beginUsage(s.chatID, false)
		if usageErr == nil {
			usageErr = s.saveUsage(s.chatID, t.inputRun.ref.Load().RunID, t.usages)
			finishUsage(usageErr)
		}
		if usageErr != nil {
			return s.failTurn(t, "usage_failed", "语音用量写入失败，费用可能不完整，请查看聊天界面")
		}
		t.phase, t.text = "running", ""
		if err := s.write(map[string]any{"type": "steer", "state": "accepted", "turn": t.number, "requestId": t.requestID, "steerId": s.operationID(t), "runId": t.inputRun.ref.Load().RunID}); err != nil {
			return err
		}
		if completed := s.completed; completed != nil {
			return s.finishStoredRun(t, completed)
		}
		return nil
	}
	if result.Status == "unmatched" {
		return s.routeInput(t) // Only explicit non-admission permits a later Query.
	}
	s.clearCompleted()
	if result.Status == "awaiting_pending" {
		return s.failTurn(t, "input_changed", "当前已有等待项，请查看聊天界面")
	}
	if result.Status == "unsupported" {
		return s.failTurn(t, "steer_rejected", "当前任务不支持语音补充，请通过聊天界面继续")
	}
	return s.failTurn(t, "steer_rejected", "当前任务未接收补充，请查看聊天界面")
}
