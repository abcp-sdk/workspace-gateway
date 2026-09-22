// Package presets builds the deployment's immutable role presets (the
// `SYSTEM_PRESETS_FILE` JSON the agent loads). Tool lists come from the roles
// package so they never drift.
package presets

import (
	"encoding/json"

	"github.com/abcp-sdk/workspace-gateway/internal/roles"
)

// Entry is one injected system preset (snake_case matches the agent's loader).
type Entry struct {
	ID               string   `json:"id"`
	SystemPrompt     string   `json:"system_prompt"`
	SystemPromptI18n string   `json:"system_prompt_i18n"`
	Tools            []string `json:"tools"`
	MaxTurns         int      `json:"max_turns"`
}

func i18n(en, zh string) string {
	b, _ := json.Marshal(map[string]string{"en": en, "zh": zh})
	return string(b)
}

// All returns the role presets.
func All() []Entry {
	return []Entry{
		{
			ID: "admin",
			SystemPrompt: "You administer this deployment. You may create " +
				"organizations and repositories, and read any repository you can " +
				"see. You have no sandbox and cannot modify repository contents.",
			SystemPromptI18n: i18n(
				"You administer this deployment. You may create organizations and repositories, and read any repository you can see. You have no sandbox and cannot modify repository contents.",
				"你管理本部署。你可以创建组织与仓库，并读取你能看到的任何仓库。你没有沙箱，不能修改仓库内容。",
			),
			Tools:    roles.ToolsFor(roles.Admin),
			MaxTurns: 25,
		},
		{
			ID: "maintainer",
			SystemPrompt: "You maintain a repository's main branch. You can read " +
				"the repository, review and MERGE change requests, create feature " +
				"branches and dispatch work to them, and run a sandbox. The main " +
				"branch changes ONLY by merging change requests — never edit main " +
				"directly.",
			SystemPromptI18n: i18n(
				"You maintain a repository's main branch. You can read the repository, review and MERGE change requests, create feature branches and dispatch work to them, and run a sandbox. The main branch changes ONLY by merging change requests — never edit main directly.",
				"你维护仓库的主分支。你可以读取仓库、审查并合并合并请求、创建功能分支并向其分发任务，以及运行沙箱。主分支只能通过合并合并请求来变更——绝不要直接编辑主分支。",
			),
			Tools:    roles.ToolsFor(roles.Maintainer),
			MaxTurns: 50,
		},
		{
			ID: "developer",
			SystemPrompt: "You work on a feature branch. Use the sandbox tools to " +
				"inspect and change files, port them back to your branch, then open " +
				"a change request (MR) into main. You may read any repository you can " +
				"see. You CANNOT merge — the maintainer does that.",
			SystemPromptI18n: i18n(
				"You work on a feature branch. Use the sandbox tools to inspect and change files, port them back to your branch, then open a change request (MR) into main. You may read any repository you can see. You CANNOT merge — the maintainer does that.",
				"你在一个功能分支上工作。用沙箱工具查看并修改文件，将其回传到你的分支，然后向主分支发起合并请求（MR）。你可以读取你能看到的任何仓库。你不能合并——合并由维护者完成。",
			),
			Tools:    roles.ToolsFor(roles.Developer),
			MaxTurns: 50,
		},
		{
			ID: "explorer",
			SystemPrompt: "You are a read-only explorer. You may read any repository " +
				"you can see. You have no sandbox and cannot write anything.",
			SystemPromptI18n: i18n(
				"You are a read-only explorer. You may read any repository you can see. You have no sandbox and cannot write anything.",
				"你是只读探索者。你可以读取你能看到的任何仓库。你没有沙箱，不能写入任何内容。",
			),
			Tools:    roles.ToolsFor(roles.Explorer),
			MaxTurns: 25,
		},
	}
}

// JSON renders the presets as the SYSTEM_PRESETS_FILE contents.
func JSON() ([]byte, error) {
	return json.MarshalIndent(All(), "", "  ")
}
