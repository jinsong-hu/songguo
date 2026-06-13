package wire

import "testing"

func TestVolcTTSResolve(t *testing.T) {
	w, ok := Resolve([]string{"volc/tts"}, "POST", "/api/v3/tts/unidirectional")
	if !ok || w.Name != "volc/tts" {
		t.Fatalf("Resolve = %q, %v; want volc/tts, true", w.Name, ok)
	}
}

func TestVolcVoiceCloneResolve(t *testing.T) {
	enabled := []string{"volc/tts", "volc/voice-clone"}
	for _, path := range []string{"/api/v3/tts/voice_clone", "/api/v3/tts/get_voice"} {
		w, ok := Resolve(enabled, "POST", path)
		if !ok || w.Name != "volc/voice-clone" {
			t.Fatalf("Resolve(%q) = %q, %v; want volc/voice-clone, true", path, w.Name, ok)
		}
		if !w.ZeroCost {
			t.Errorf("volc/voice-clone should be ZeroCost")
		}
	}
}

func TestVolcTTSExtract(t *testing.T) {
	body := []byte(`{"code":0,"message":"OK","data":"...","usage":{"text_words":7}}`)
	got := volcTTSExtract(body, nil)
	if got.Norm.Chars != 7 {
		t.Errorf("Chars = %v, want 7", got.Norm.Chars)
	}
	if got.Norm.InputTokens != 0 || got.Norm.OutputTokens != 0 {
		t.Errorf("tokens should be zero, got %+v", got.Norm)
	}
}

func TestVolcTTSScanner(t *testing.T) {
	s := newVolcTTSScanner(nil)
	// Chunked JSON lines, split across arbitrary write boundaries; usage
	// arrives on the final chunk.
	writes := []string{
		"{\"code\":0,\"data\":\"abc\"}\n{\"code\":0,",
		"\"data\":\"def\"}\n",
		"{\"code\":0,\"usage\":{\"text_words\":42}}\n",
	}
	for _, w := range writes {
		if _, err := s.Write([]byte(w)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := s.Result()
	if got.Norm.Chars != 42 {
		t.Errorf("Chars = %v, want 42", got.Norm.Chars)
	}
}
