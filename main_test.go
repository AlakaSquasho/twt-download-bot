package main

import (
	"os"
	"testing"
)

func unsetenv(t *testing.T, key string) {
	t.Helper()
	orig, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unset %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, orig)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestLoadConfigTranslateLang(t *testing.T) {
	t.Setenv("BOT_TOKEN", "123:abc")
	t.Setenv("ALLOWED_USER_IDS", "1")
	t.Setenv("STATE_DIR", t.TempDir())
	t.Setenv("TMP_DIR", t.TempDir())

	tests := []struct {
		name    string
		set     string
		unset   bool
		want    string
		wantErr bool
	}{
		{name: "default", unset: true, want: "zh-Hans"},
		{name: "disabled", set: "", want: ""},
		{name: "custom", set: "ja", want: "ja"},
		{name: "regional", set: "zh-Hans", want: "zh-Hans"},
		{name: "invalid", set: "zh/Hans", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unset {
				unsetenv(t, "TRANSLATE_LANG")
			} else {
				t.Setenv("TRANSLATE_LANG", tt.set)
			}

			cfg, err := loadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if cfg.translateLang != tt.want {
				t.Errorf("translateLang = %q, want %q", cfg.translateLang, tt.want)
			}
		})
	}
}
