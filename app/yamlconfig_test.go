package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/tg-spam/app/config"
)

func TestApplyYAMLOverlay(t *testing.T) {
	t.Run("empty path is no-op", func(t *testing.T) {
		s := &config.Settings{}
		s.Telegram.Group = "preserved"
		require.NoError(t, applyYAMLOverlay("", s))
		assert.Equal(t, "preserved", s.Telegram.Group)
		assert.Empty(t, s.Telegram.Groups)
	})

	t.Run("populates Telegram.Groups", func(t *testing.T) {
		f := writeTempYAML(t, `
telegram:
  groups:
    - group: "-1001"
      gid: test1
    - group: "-1002"
      gid: test2
`)
		s := &config.Settings{}
		require.NoError(t, applyYAMLOverlay(f, s))
		require.Len(t, s.Telegram.Groups, 2)
		assert.Equal(t, "-1001", s.Telegram.Groups[0].Group)
		assert.Equal(t, "test1", s.Telegram.Groups[0].GID)
		assert.Equal(t, "-1002", s.Telegram.Groups[1].Group)
		assert.Equal(t, "test2", s.Telegram.Groups[1].GID)
	})

	t.Run("groups overwrite CLI-supplied list when present", func(t *testing.T) {
		f := writeTempYAML(t, `
telegram:
  groups:
    - group: "from-yaml"
      gid: yaml1
`)
		s := &config.Settings{}
		s.Telegram.Groups = []config.ConfiguredChat{{Group: "from-cli", GID: "cli1"}}
		require.NoError(t, applyYAMLOverlay(f, s))
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "from-yaml", s.Telegram.Groups[0].Group)
	})

	t.Run("empty groups list leaves existing untouched", func(t *testing.T) {
		f := writeTempYAML(t, `
admin:
  superusers_cross_chat: true
`)
		s := &config.Settings{}
		s.Telegram.Groups = []config.ConfiguredChat{{Group: "kept", GID: "k"}}
		require.NoError(t, applyYAMLOverlay(f, s))
		require.Len(t, s.Telegram.Groups, 1)
		assert.Equal(t, "kept", s.Telegram.Groups[0].Group)
		assert.True(t, s.Admin.SuperUsersCrossChat)
	})

	t.Run("superusers_cross_chat propagated", func(t *testing.T) {
		f := writeTempYAML(t, `
admin:
  superusers_cross_chat: true
`)
		s := &config.Settings{}
		require.NoError(t, applyYAMLOverlay(f, s))
		assert.True(t, s.Admin.SuperUsersCrossChat)
	})

	t.Run("missing file errors", func(t *testing.T) {
		err := applyYAMLOverlay(filepath.Join(t.TempDir(), "nope.yml"), &config.Settings{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "read config file")
	})

	t.Run("malformed yaml errors", func(t *testing.T) {
		f := writeTempYAML(t, "telegram: [not, a, map")
		err := applyYAMLOverlay(f, &config.Settings{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse config file")
	})

	t.Run("unknown key rejected", func(t *testing.T) {
		f := writeTempYAML(t, `
telegram:
  groups:
    - group: "g"
      gid: "x"
  unknown_field: 42
`)
		err := applyYAMLOverlay(f, &config.Settings{})
		require.Error(t, err)
	})
}

func writeTempYAML(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}
