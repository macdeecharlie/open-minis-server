package main

import (
	_ "embed"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The prompt text is embedded so the server, not the APK, owns the agent's
// instructions. It is the official OpenMinis base prompt adapted for the
// dedicated Debian execution container: package management, shell dialect and
// Python/Java guidance refer to apt/dpkg instead of Alpine's apk.
//
//go:embed prompt_base.txt
var promptBase string

//go:embed prompt_browser.txt
var promptBrowserSection string

//go:embed prompt_url_browser.txt
var promptURLBrowser string

//go:embed prompt_url_plain.txt
var promptURLPlain string

//go:embed prompt_memory_on.txt
var promptMemoryOn string

//go:embed prompt_memory_off.txt
var promptMemoryOff string

const (
	helperSubagentBullet = "- subagent_task: Delegate a self-contained task to a sub agent that runs its own tool loop in an isolated hidden session, and inspect or stop the ones you started. Full contract in the tool schema. Two things it does not say: the `<agent_callback>` result and progress messages are written by the system, not typed by the user; and while a sub agent runs the user sees it in the tool bar and can watch or stop it.\n"
	memoryWriteBullet    = "- memory_write: Save a memory entry to today's daily log (YYYY-MM-DD.md). Use proactively to note user preferences, project patterns, and important context.\n- memory_get: Recall memories with keyword search. Check memory at the start of new topics to leverage past knowledge."
	userAskBullet        = "- user_ask: Ask the user questions on an in-chat card."
)

// PromptContext is the non-prompt state the agent UI owns (identity/personality,
// enabled switches, and the already-selected skills/MCP/memory fragments). The
// server owns all prompt wording and ordering; the client never sends a
// pre-built system prompt in this mode.
type PromptContext struct {
	Identity          string `json:"identity,omitempty"`
	Roster            string `json:"roster,omitempty"`
	Skills            string `json:"skills,omitempty"`
	MCP               string `json:"mcp,omitempty"`
	GlobalMemory      string `json:"global_memory,omitempty"`
	DailyMemory       string `json:"daily_memory,omitempty"`
	MemoryEnabled     *bool  `json:"memory_enabled,omitempty"`
	BrowserEnabled    bool   `json:"browser_enabled,omitempty"`
	UserAskEnabled    bool   `json:"user_ask_enabled,omitempty"`
	DelegationEnabled bool   `json:"delegation_enabled,omitempty"`
	Helper            bool   `json:"helper,omitempty"`
	Headless          bool   `json:"headless,omitempty"`
	Language          string `json:"language,omitempty"`
	Timezone          string `json:"timezone,omitempty"`
	ModelUseCount     int    `json:"model_use_count,omitempty"`
}

var officialIdentity = regexp.MustCompile(`You are (.+?), a capable AI assistant running on an Android device with a fully functional Linux sandbox \(Alpine Linux via PRoot, aarch64\)\.`)

func defaultIdentity() string {
	return "You are Minis, a capable AI assistant running in a dedicated full Debian Linux container (root) on your user's personal OpenMinis server. "
}

// buildSystemPrompt reproduces the official buildSystemPrompt ordering:
// identity + base, skills, MCP, global memory, recent daily memory, then the
// cache-friendly runtime suffix last.
func buildSystemPrompt(pc *PromptContext, workspace string) string {
	if pc == nil {
		pc = &PromptContext{}
	}
	identity := pc.Identity
	if strings.TrimSpace(identity) == "" {
		identity = defaultIdentity()
	} else {
		// The client's SOUL identity sentence still describes the old on-device
		// PRoot sandbox. Keep the user's name/personality byte-for-byte and
		// replace only the environment clause.
		identity = officialIdentity.ReplaceAllString(identity,
			"You are $1, a capable AI assistant running in a dedicated full Debian Linux container (root) on your user's personal OpenMinis server.")
	}

	memoryOn := true
	if pc.MemoryEnabled != nil {
		memoryOn = *pc.MemoryEnabled
	}
	memoryBullets := ""
	if memoryOn {
		memoryBullets = memoryWriteBullet
	}
	helperBullet := ""
	if pc.DelegationEnabled && !pc.Helper {
		helperBullet = helperSubagentBullet
	}
	toolList := helperBullet + pc.Roster + memoryBullets

	browserBullet := ""
	if pc.BrowserEnabled {
		browserBullet = strings.Replace(promptBrowserSection, "${toolListMemoryBullets}", toolList, 1)
	}
	userAskLine := ""
	if pc.UserAskEnabled {
		if browserBullet == "" {
			userAskLine = userAskBullet
		} else {
			userAskLine = "\n" + userAskBullet
		}
	}
	minisURL := promptURLPlain
	if pc.BrowserEnabled {
		minisURL = promptURLBrowser
	}
	memorySystem := promptMemoryOff
	if memoryOn {
		memorySystem = promptMemoryOn
	}
	modelUseNote := ""
	scheduledNote := ""
	if pc.DelegationEnabled && !pc.Helper {
		scheduledNote = " (Do NOT run minis-scheduled to delegate work you could hand to subagent_task.)"
		modelUseNote = " For delegating a whole task that needs tools and multiple rounds, use the subagent_task tool instead."
	}

	base := strings.NewReplacer(
		"{{BROWSER_BULLET}}", browserBullet,
		"{{USER_ASK_LINE}}", userAskLine,
		"{{MINIS_URL_NOTE}}", minisURL,
		"{{MEMORY_SYSTEM}}", memorySystem,
		"{{MODEL_USE_NOTE}}", modelUseNote,
		"{{SCHEDULED_NOTE}}", scheduledNote,
	).Replace(promptBase)

	var out strings.Builder
	out.Grow(len(identity) + len(base) + len(pc.Skills) + len(pc.MCP) + len(pc.GlobalMemory) + len(pc.DailyMemory) + 512)
	out.WriteString(identity)
	out.WriteString(base)
	appendFragment := func(s string) {
		if s == "" {
			return
		}
		out.WriteString("\n\n")
		out.WriteString(s)
	}
	appendFragment(pc.Skills)
	appendFragment(pc.MCP)
	appendFragment(pc.GlobalMemory)
	appendFragment(pc.DailyMemory)

	tz := strings.TrimSpace(pc.Timezone)
	loc := time.UTC
	if tz != "" {
		if loaded, err := time.LoadLocation(tz); err == nil {
			loc = loaded
		}
	} else {
		tz = "UTC"
	}
	lang := strings.TrimSpace(pc.Language)
	if lang == "" {
		lang = "en"
	}
	out.WriteString("\n\nRuntime context:\n")
	out.WriteString("- Current date: " + time.Now().In(loc).Format("2006-01-02") + " (" + tz + ")\n")
	out.WriteString("- Device language: " + lang + "\n")
	out.WriteString("- minis-model-use models available: " + strconv.Itoa(pc.ModelUseCount) + "\n")
	out.WriteString("- Execution environment: dedicated Debian 12 container, root shell, workspace " + workspace + " (maps /var/minis/workspace and /var/minis/attachments)")
	if pc.Headless {
		out.WriteString("\n- This is a headless server run: Android/device tools, minis-scheduled, memory tools, user_ask, browser automation and the interactive terminal are unavailable. Use shell_execute, file tools and web tools only; do not ask the user questions.")
	}
	return out.String()
}
