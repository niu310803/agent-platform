package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-platform/internal/api"
	"agent-platform/internal/chat"
	"agent-platform/internal/conversation"
	"agent-platform/internal/models"
	"agent-platform/internal/modelusage"
	"github.com/gorilla/websocket"
)

func TestVoiceUsageReadProjectionArchiveRestoreAndExport(t *testing.T) {
	s, active, _ := newArchiveHandlerTestServer(t, nil)
	defer active.Close()
	seedArchiveHandlerChat(t, active, "voice-cost")
	base, _ := active.Summary("voice-cost")
	finish, err := s.conversationService().BeginVoiceUsage("voice-cost")
	if err != nil {
		t.Fatal(err)
	}
	u := chat.VoiceUsage{UsageID: "tts", TaskID: "tts", Provider: "provider", Model: "tts", RunID: "run-voice-cost", Operation: modelusage.OperationSpeech, Known: true, Characters: 10000, Pricing: models.ModelPricing{Currency: "CNY", Unit: models.PricingUnitPer10KCharacters, Characters: 2}, Status: "complete"}
	if _, err := s.conversationService().AppendVoiceUsage("voice-cost", u); err != nil {
		t.Fatal(err)
	}
	u = chat.VoiceUsage{UsageID: "asr", TaskID: "asr", Provider: "provider", Model: "asr", Operation: modelusage.OperationRecognition, Status: "cancelled"}
	if _, err := s.conversationService().AppendVoiceUsage("voice-cost", u); err != nil {
		t.Fatal(err)
	}
	finish(nil)
	check := func(usage *api.ChatUsageData) {
		t.Helper()
		if usage == nil || usage.EstimatedCost == nil || usage.EstimatedCost.Total != 2 || usage.TotalTokens != 8 || usage.Voice == nil || usage.Voice.Tasks != 2 || usage.Voice.UnknownTasks != 1 {
			t.Fatalf("incorrect projection: %+v", usage)
		}
	}
	detail, err := s.loadChatDetail(context.Background(), "voice-cost", false)
	if err != nil {
		t.Fatal(err)
	}
	check(detail.Usage.Chat)
	if detail.Usage.LastRun.Voice.Tasks != 1 || detail.Runs[0].Usage.Voice.Tasks != 1 || detail.Runs[0].Usage.EstimatedCost.Total != 2 {
		t.Fatal("Chat-only ASR entered Run cost", detail.Usage)
	}
	after, _ := active.Summary("voice-cost")
	if *after.Usage != *base.Usage || after.UpdatedAt != base.UpdatedAt {
		t.Fatal("projection modified SQL base")
	}
	rawBefore, err := s.loadChatJSONLContent("voice-cost")
	if err != nil || strings.Count(rawBefore, `"type":"voice.usage"`) != 2 {
		t.Fatal("export omitted facts", err)
	}
	result, err := s.archiveChats([]string{"voice-cost"})
	if err != nil || !result.Results[0].Success {
		t.Fatal(result, err)
	}
	archived, err := s.loadArchiveDetail(context.Background(), "voice-cost", false)
	if err != nil {
		t.Fatal(err)
	}
	check(archived.Usage)
	list, err := s.listArchives(api.ArchivesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	check(list.Items[0].Usage)
	rawAfter, err := s.loadChatJSONLContent("voice-cost")
	if err != nil || rawAfter != rawBefore {
		t.Fatal("archival changed accounting export", err)
	}
	restored, err := s.restoreArchives([]string{"voice-cost"})
	if err != nil {
		t.Fatal(err)
	}
	check(restored.Results[0].Summary.Usage)
	detail, err = s.loadChatDetail(context.Background(), "voice-cost", false)
	if err != nil {
		t.Fatal(err)
	}
	check(detail.Usage.Chat)
}

func TestVoiceUsageHTTPDeleteRejectsInflight(t *testing.T) {
	s, active, _ := newArchiveHandlerTestServer(t, nil)
	defer active.Close()
	seedArchiveHandlerChat(t, active, "voice-delete")
	finish, err := s.conversationService().BeginVoiceUsage("voice-delete")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{http.StatusConflict, http.StatusOK} {
		rec := httptest.NewRecorder()
		s.handleChatDelete(rec, httptest.NewRequest(http.MethodDelete, "/api/chat?chatId=voice-delete", nil))
		if rec.Code != want {
			t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
		}
		finish(nil)
	}
}

func TestVoiceUsageProjectionDoesNotHideCurrencyFailure(t *testing.T) {
	_, err := chatUsageBreakdown(&chat.UsageData{EstimatedCostCurrency: "USD", EstimatedCostTotal: 1}, nil, chat.ReplayUsage{VoiceChat: chat.VoiceUsageTotals{Tasks: 1, Cost: &modelusage.EstimatedCost{Currency: "CNY", Total: 2}}})
	if err == nil {
		t.Fatal("mixed currencies produced a false total")
	}
}

func TestVoiceUsageWSDeleteRejectsInflight(t *testing.T) {
	f := connectorUsageFixture(t)
	if _, _, err := f.chats.EnsureChat("voice-delete", "mock-agent", "hello"); err != nil {
		t.Fatal(err)
	}
	finish, err := f.server.conversationService().BeginVoiceUsage("voice-delete")
	if err != nil {
		t.Fatal(err)
	}
	defer finish(nil)
	c := connectorUsageSocket(t, f.server)
	sendConnectorUsageRequest(t, c, "/api/chat/delete", "delete", map[string]any{"chatId": "voice-delete"})
	if frame := readConnectorUsageError(t, c, "delete"); frame.Code != 409 || frame.Type != "voice_usage_pending" {
		t.Fatal(frame)
	}
}

func TestVoiceUsageNewChatBindsActualRunAndGuardsTTS(t *testing.T) {
	f := newTestFixture(t)
	srv, provider := enableVoiceFixture(t, f, nil)
	provider.reportUsage.Store(true)
	provider.holdTTS.Store(true)
	defer close(provider.releaseTTS)
	c, chatID := dialVoice(t, srv, "", 16000)
	speakVoice(t, c)
	voiceUntil(t, c, "tts", "start")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		kind, _, err := c.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.BinaryMessage {
			break
		}
	}

	if err := f.server.conversationService().DeleteChat(chatID); !errors.Is(err, conversation.ErrVoiceUsagePending) {
		t.Fatal("TTS not protected", err)
	}

	// The provider has reported characters, then blocks after the first PCM frame. Closing the
	// media connection must retain them without cancelling/replaying the Run.
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		detail, err := f.chats.LoadChat(chatID)
		if err != nil {
			t.Fatal(err)
		}
		if detail.ReplayUsage.VoiceChat.Tasks == 2 {
			voice := detail.ReplayUsage.VoiceChat
			if voice.Characters != 12 || voice.BilledAudioSeconds != 3 || voice.UnknownTasks != 0 || voice.UnpricedTasks != 2 {
				t.Fatalf("wrong billed quantities: %+v", voice)
			}
			runs, _ := f.chats.ListRuns(chatID)
			if len(runs) != 1 || detail.ReplayUsage.VoiceByRun[runs[0].RunID].Tasks != 2 || runs[0].FinishReason != "complete" {
				t.Fatal("Run binding changed lifecycle", runs)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("disconnect lost provider usage")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestVoiceUsageEmptyASROnExistingChatHasNoRun(t *testing.T) {
	f := newTestFixture(t)
	if _, _, err := f.chats.EnsureChat("voice-empty", "mock-agent", "hello"); err != nil {
		t.Fatal(err)
	}
	srv, provider := enableVoiceFixture(t, f, nil)
	provider.transcript.Store("")
	provider.reportUsage.Store(true)
	c, _ := dialVoice(t, srv, "voice-empty", 16000)
	speakVoice(t, c)
	voiceUntil(t, c, "stt", "")
	detail, err := f.chats.LoadChat("voice-empty")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ReplayUsage.VoiceChat.BilledAudioSeconds != 3 || len(detail.ReplayUsage.VoiceByRun) != 0 || detail.ReplayUsage.LastRunID != "" {
		t.Fatalf("ASR-only attribution: %+v", detail.ReplayUsage)
	}
}
