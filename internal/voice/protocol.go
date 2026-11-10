package voice

import (
	"agent-platform/internal/contracts/queryinput"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agent-platform/internal/chat"
	"github.com/google/uuid"
)

type audioFormat struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sampleRate"`
	Channels   int    `json:"channels,omitempty"`
	FrameMS    int    `json:"frameMs,omitempty"`
	PreSkip48k int    `json:"preSkip48k,omitempty"`
}

type clientMessage struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	AgentKey string `json:"agentKey,omitempty"`
	ChatID   string `json:"chatId,omitempty"`
	NewChat  bool   `json:"newChat,omitempty"`
	Primary  *bool  `json:"primary,omitempty"`
	TimeZone string `json:"timeZone,omitempty"`
	Voice    string `json:"voice,omitempty"`
	Audio    struct {
		Input  audioFormat `json:"input"`
		Output audioFormat `json:"output"`
	} `json:"audio,omitempty"`
	State      string `json:"state,omitempty"`
	RequestID  string `json:"requestId,omitempty"`
	Turn       int    `json:"turn,omitempty"`
	Intent     string `json:"intent,omitempty"`
	RunID      string `json:"runId,omitempty"`
	AwaitingID string `json:"awaitingId,omitempty"`
	QuestionID string `json:"questionId,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func decodeMessage(data []byte) (clientMessage, error) {
	var msg clientMessage
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return msg, errors.New("invalid voice message")
	}
	if err := queryinput.DecodeStrictJSON(data, &msg); err != nil {
		return msg, errors.New("invalid voice message")
	}
	allowed := []string{"type"}
	switch msg.Type {
	case "hello":
		allowed = append(allowed, "version", "agentKey", "chatId", "newChat", "primary", "voice", "audio", "timeZone")
	case "listen":
		allowed = append(allowed, "state", "requestId", "turn")
		if msg.State == "start" {
			allowed = append(allowed, "intent", "runId", "awaitingId", "questionId")
		}
		if msg.State != "start" && msg.State != "stop" || msg.Turn < 1 || msg.Turn > 9007199254740991 || strings.TrimSpace(msg.RequestID) == "" || len(msg.RequestID) > 128 {
			return msg, errors.New("invalid listen control")
		}
		if msg.State == "start" {
			if msg.Intent != "conversation" && msg.Intent != "question" {
				return msg, errors.New("invalid listen intent")
			}
			if msg.Intent == "question" {
				if msg.RunID == "" || msg.AwaitingID == "" || msg.QuestionID == "" {
					return msg, errors.New("question binding required")
				}
			} else if _, ok := fields["runId"]; ok {
				return msg, errors.New("unexpected conversation binding")
			} else if _, ok := fields["awaitingId"]; ok {
				return msg, errors.New("unexpected conversation binding")
			} else if _, ok := fields["questionId"]; ok {
				return msg, errors.New("unexpected conversation binding")
			}
		}
	case "abort":
		allowed = append(allowed, "reason")
	case "close":
	default:
		return msg, errors.New("unsupported voice control")
	}
	if err := queryinput.ValidateFields("voice", fields, allowed...); err != nil {
		return msg, errors.New("invalid voice fields")
	}
	return msg, nil
}

func (s *Service) resolveHello(msg clientMessage) (string, bool, string, error) {
	if msg.Type != "hello" || msg.Version != 2 || msg.AgentKey == "" {
		return "", false, "", errors.New("invalid voice hello")
	}
	if msg.TimeZone == "" || msg.TimeZone == "Local" {
		return "", false, "", errors.New("valid IANA timeZone required")
	}
	if _, err := time.LoadLocation(msg.TimeZone); err != nil {
		return "", false, "", errors.New("valid IANA timeZone required")
	}
	if msg.NewChat && msg.ChatID != "" || msg.Primary != nil && !msg.NewChat {
		return "", false, "", errors.New("invalid voice chat intent")
	}
	if _, ok := s.deps.Agents.AgentDefinition(msg.AgentKey); !ok {
		return "", false, "", errors.New("agent unavailable")
	}
	in, out := msg.Audio.Input, msg.Audio.Output
	if !in.validInput() || in.Codec == "opus" && !s.opusInputSupported() || out.Codec != "pcm_s16le" || (out.SampleRate != 16000 && out.SampleRate != 24000) || (out.Channels != 0 && out.Channels != 1) || (out.FrameMS != 0 && out.FrameMS != 20) || out.PreSkip48k != 0 {
		return "", false, "", errors.New("unsupported voice audio format")
	}
	voice := msg.Voice
	if voice == "" {
		voice = s.deps.Config.DefaultVoice
	}
	allowed := false
	for _, id := range s.deps.Config.Voices {
		allowed = allowed || id == voice
	}
	if !allowed {
		return "", false, "", errors.New("voice not allowed")
	}
	if msg.NewChat {
		// Reserve only; the first nonempty transcript creates the Chat through Runtime.
		return uuid.NewString(), msg.Primary != nil && *msg.Primary, voice, nil
	}
	chatID := msg.ChatID
	var err error
	if chatID == "" {
		chatID, err = s.deps.Chats.PrimaryChatID(msg.AgentKey)
	}
	if err != nil {
		return "", false, "", errors.New("chat unavailable")
	}
	if chatID == "" {
		return uuid.NewString(), true, voice, nil
	}
	if !chat.ValidChatID(chatID) {
		return "", false, "", errors.New("invalid chatId")
	}
	summary, err := s.deps.Chats.Summary(chatID)
	if err != nil || summary == nil || summary.AgentKey != msg.AgentKey || chat.IsPendingUploadChat(*summary) {
		return "", false, "", errors.New("chat does not belong to agent")
	}
	return chatID, false, voice, nil
}

func (f audioFormat) validInput() bool {
	if f.SampleRate != 16000 || f.Channels != 1 {
		return false
	}
	switch f.Codec {
	case "pcm_s16le":
		return f.FrameMS == 20 && f.PreSkip48k == 0
	case "opus":
		return f.FrameMS == 0 && f.PreSkip48k >= 0 && f.PreSkip48k <= 65535
	default:
		return false
	}
}

func (f audioFormat) samples(data []byte) (int, error) {
	if f.Codec == "opus" {
		return OpusPacketSamples(data)
	}
	if len(data) == 0 || len(data) > 640 || len(data)%2 != 0 {
		return 0, errors.New("invalid voice PCM frame")
	}
	return len(data) / 2 * 3, nil // 16kHz PCM expressed in the common 48kHz clock.
}

func (f audioFormat) startupFrames() int {
	if f.Codec == "opus" {
		return int(maxStartupAudio / (2500 * time.Microsecond))
	}
	return int(maxStartupAudio / (20 * time.Millisecond))
}
