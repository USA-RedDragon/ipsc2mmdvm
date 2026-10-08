package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/USA-RedDragon/configulator/v2"
	"github.com/goccy/go-yaml"
)

func TestLoadFileAppliesElementDefaults(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte(`mmdvm:
  - name: BM
    tg-rewrite:
      - from-slot: 1
        from-tg: 9
        to-slot: 2
        to-tg: 100
    pc-rewrite:
      - from-slot: 1
        from-id: 100
        to-slot: 1
        to-id: 200
        range: 5
    type-rewrite:
      - from-slot: 1
        from-tg: 9
        to-slot: 2
        to-id: 3100
    src-rewrite:
      - from-slot: 1
        from-id: 1234
        to-slot: 2
        to-id: 9
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := configulator.New(ConfigSchema()).
		WithFile(&configulator.FileOptions{
			Search:       []string{path},
			RequireFound: true,
			Decoders:     configulator.Decoders{".yaml": yaml.Unmarshal},
		}).
		LoadWithoutValidation()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.MMDVM) != 1 {
		t.Fatalf("expected 1 mmdvm entry, got %d", len(cfg.MMDVM))
	}
	m := cfg.MMDVM[0]
	if m.Slots != 3 {
		t.Errorf("expected slots default 3, got %d", m.Slots)
	}
	if len(m.TGRewrites) != 1 || m.TGRewrites[0].Range != 1 || m.TGRewrites[0].ToTG != 100 {
		t.Errorf("unexpected tg-rewrite: %+v", m.TGRewrites)
	}
	if len(m.PCRewrites) != 1 || m.PCRewrites[0].Range != 5 || m.PCRewrites[0].ToID != 200 {
		t.Errorf("unexpected pc-rewrite: %+v", m.PCRewrites)
	}
	if len(m.TypeRewrites) != 1 || m.TypeRewrites[0].Range != 1 || m.TypeRewrites[0].ToID != 3100 {
		t.Errorf("unexpected type-rewrite: %+v", m.TypeRewrites)
	}
	if len(m.SrcRewrites) != 1 || m.SrcRewrites[0].Range != 1 || m.SrcRewrites[0].FromID != 1234 {
		t.Errorf("unexpected src-rewrite: %+v", m.SrcRewrites)
	}
}
