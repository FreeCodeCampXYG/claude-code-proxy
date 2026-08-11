package server

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestEmbeddedLocalPageScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is required for embedded page script syntax checks")
	}
	pages := map[string]string{
		"dashboard":  dashboardPageHTML,
		"monitor":    monitorPageHTML,
		"settings":   settingsPageHTML,
		"prompts":    promptsPageHTML,
		"playground": playgroundPageHTML,
		"diagnostics": diagnosticsPageHTML,
	}
	scriptPattern := regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`)
	for name, page := range pages {
		page = strings.NewReplacer(
			"__PLAYGROUND_BOOTSTRAP__", "{}",
			"__MONITOR_BOOTSTRAP__", `{"snapshot":{}}`,
			"__SETTINGS_BOOTSTRAP__", "{}",
			"__PROMPTS_BOOTSTRAP__", "{}",
			"__DASHBOARD_TITLE__", "Console",
			"__DASHBOARD_STATUS__", "Status",
			"__DASHBOARD_CARDS__", "",
			"__DASHBOARD_JSON__", "{}",
			"__LOCAL_PAGE_TITLE__", "Page",
			"__LOCAL_PAGE_DESC__", "Description",
		).Replace(page)
		scripts := scriptPattern.FindAllStringSubmatch(page, -1)
		if len(scripts) == 0 {
			t.Errorf("%s has no inline scripts", name)
			continue
		}
		for index, match := range scripts {
			cmd := exec.Command("node", "-e", "new (require('vm').Script)(require('fs').readFileSync(0, 'utf8'))")
			cmd.Stdin = strings.NewReader(match[1])
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s inline script %d does not parse: %v\n%s", name, index+1, err, output)
			}
		}
	}
}
