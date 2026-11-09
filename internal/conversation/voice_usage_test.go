package conversation

import (
	"errors"
	"testing"

	"agent-platform/internal/chat"
	"agent-platform/internal/modelusage"
)

func TestVoiceAccountingSharesMutationBoundaryAndPreservesArchiveBase(t *testing.T) {
	root := t.TempDir()
	active, err := chat.NewFileStoreAtStartup(root)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	archive, err := chat.NewArchiveStoreAtStartup(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = active.EnsureChat("voice", "agent", "hello"); err != nil {
		t.Fatal(err)
	}
	controlAppendRun(t, active, "voice", "completed-run", "done")
	s := NewService(active, archive, chat.NewArchiver(active, archive), nil)
	sink := &primaryTestSink{}
	s.Notifications = sink
	view := s.WithDependencies(nil, nil, nil, nil, sink)
	finish, err := view.BeginVoiceUsage("voice")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteChat("voice"); !errors.Is(err, ErrVoiceUsagePending) {
		t.Fatal("shared view did not guard delete", err)
	}
	results, err := s.ArchiveChats([]string{"voice"})
	if err != nil || len(results) != 1 || results[0].Success || results[0].Error != ErrVoiceUsagePending.Error() {
		t.Fatal("in-flight archive accepted", results, err)
	}
	u := chat.VoiceUsage{UsageID: "unknown", Provider: "provider", TaskID: "task", Model: "asr", Operation: modelusage.OperationRecognition, Status: "cancelled"}
	if _, err = view.AppendVoiceUsage("voice", u); err != nil {
		t.Fatal(err)
	}
	finish(nil)
	finish(errors.New("late duplicate"))
	results, err = s.ArchiveChats([]string{"voice"})
	if err != nil || !results[0].Success {
		t.Fatal("unknown terminal kept hold", results, err)
	}
	archived, err := s.LoadArchive("voice")
	if err != nil {
		t.Fatal(err)
	}
	if archived.Summary.Usage != nil || archived.Detail.ReplayUsage.VoiceChat.UnknownTasks != 1 {
		t.Fatal("archive changed base or lost voice usage")
	}
	if _, err = s.RestoreArchives([]string{"voice"}); err != nil {
		t.Fatal(err)
	}
	detail, err := active.LoadChat("voice")
	if err != nil || detail.ReplayUsage.VoiceChat.UnknownTasks != 1 {
		t.Fatal("restore lost voice fact", err)
	}
	finish, err = s.BeginVoiceUsage("voice")
	if err != nil {
		t.Fatal(err)
	}
	finish(errors.New("write failed"))
	if err = s.DeleteChat("voice"); !errors.Is(err, ErrVoiceUsagePending) {
		t.Fatal("write failure silently released hold", err)
	}
}
