package main

import (
	_ "embed"
	"fmt"
)

// skillText é o skill.md: como um agente de IA deve usar o pgb, no formato de skill do Claude Code
// (pgb --skill > .claude/skills/pgb/SKILL.md).
//
//go:embed skill.md
var skillText string

func printSkill() { fmt.Print(skillText) }
