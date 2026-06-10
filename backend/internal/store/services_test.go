package store

import (
	"testing"
)

func TestServiceCRUDRoundTrip(t *testing.T) {
	s := openTestStore(t)

	svc, err := s.CreateService(NewService{
		Name:     "openai",
		Vendor:   "OpenAI",
		Adapter:  "openai-compatible",
		BaseURL:  "https://api.openai.com/v1",
		Priority: 1,
		Weight:   2,
		Enabled:  true,
		APIKeys:  []string{"sk-aaa", "sk-bbb"},
		Models: []ServiceModel{
			{Model: "gpt-4o", Input: 2.5, Output: 10, Unit: "per_1m_tokens"},
			{Model: "gpt-4o-mini", Input: 0.15, Output: 0.6, Unit: "per_1m_tokens"},
		},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	if svc.ID == "" {
		t.Fatal("expected generated service id")
	}
	if len(svc.Credentials) != 2 {
		t.Fatalf("credentials = %d, want 2", len(svc.Credentials))
	}
	if len(svc.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(svc.Models))
	}
	if svc.Weight != 2 {
		t.Errorf("weight = %d, want 2", svc.Weight)
	}

	// Duplicate name must fail (UNIQUE).
	if _, err := s.CreateService(NewService{Name: "openai", BaseURL: "https://x.example.com"}); err == nil {
		t.Error("expected duplicate name to fail")
	}

	// Update scalar + replace models.
	newName := "openai-main"
	disabled := false
	updated, err := s.UpdateService(svc.ID, ServiceUpdate{
		Name:    &newName,
		Enabled: &disabled,
		Models:  []ServiceModel{{Model: "gpt-4o", Input: 3, Output: 12, Unit: "per_1m_tokens"}},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if updated.Name != "openai-main" {
		t.Errorf("name = %q, want openai-main", updated.Name)
	}
	if updated.Enabled {
		t.Error("expected disabled")
	}
	if len(updated.Models) != 1 || updated.Models[0].Input != 3 {
		t.Errorf("models not replaced: %+v", updated.Models)
	}

	// Add + delete a credential.
	cred, err := s.AddCredential(svc.ID, "sk-ccc")
	if err != nil {
		t.Fatalf("AddCredential: %v", err)
	}
	got, _ := s.GetService(svc.ID)
	if len(got.Credentials) != 3 {
		t.Fatalf("after add credentials = %d, want 3", len(got.Credentials))
	}
	if err := s.DeleteCredential(svc.ID, cred.ID); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	got, _ = s.GetService(svc.ID)
	if len(got.Credentials) != 2 {
		t.Fatalf("after delete credentials = %d, want 2", len(got.Credentials))
	}

	// List + count.
	if n, _ := s.CountServices(); n != 1 {
		t.Errorf("CountServices = %d, want 1", n)
	}
	list, err := s.ListServices()
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	if len(list) != 1 || len(list[0].Credentials) != 2 || len(list[0].Models) != 1 {
		t.Errorf("ListServices assembly wrong: %+v", list)
	}

	// Delete cascades.
	if err := s.DeleteService(svc.ID); err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if _, err := s.GetService(svc.ID); err == nil {
		t.Error("expected service gone")
	}
	if n, _ := s.CountServices(); n != 0 {
		t.Errorf("CountServices after delete = %d, want 0", n)
	}
}

func TestAppSettingsRoundTrip(t *testing.T) {
	s := openTestStore(t)

	as, err := s.GetAppSettings()
	if err != nil {
		t.Fatalf("GetAppSettings: %v", err)
	}
	if as.Capture {
		t.Error("capture should default off")
	}
	if as.CaptureMaxBytes != 32768 || as.CaptureRetain != 10000 {
		t.Errorf("defaults wrong: %+v", as)
	}

	if err := s.UpdateAppSettings(AppSettings{Capture: true, CaptureMaxBytes: 1000, CaptureRetain: 50}); err != nil {
		t.Fatalf("UpdateAppSettings: %v", err)
	}
	as, _ = s.GetAppSettings()
	if !as.Capture || as.CaptureMaxBytes != 1000 || as.CaptureRetain != 50 {
		t.Errorf("settings not persisted: %+v", as)
	}
}
