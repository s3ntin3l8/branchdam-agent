#!/usr/bin/env bash
# Claude Code reads .claude/skills and .claude/agents; Codex, opencode and agy read .agents/skills.
# The two trees carry the same instructions, so fail when a copy drifts:
#   .claude/skills/<n>/SKILL.md  must equal  .agents/skills/<n>/SKILL.md
#   .claude/agents/<n>.md        body (after frontmatter) must equal that of .agents/skills/<n>/SKILL.md
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

body() { awk 'NR==1 && $0=="---" {fm=1; next} fm && $0=="---" {fm=0; next} !fm && (started || NF) {started=1; print}' "$1"; }

fail=0
for f in .claude/skills/*/SKILL.md; do
  [ -e "$f" ] || continue
  m=".agents/skills/$(basename "$(dirname "$f")")/SKILL.md"
  if [ ! -f "$m" ]; then echo "missing mirror: $m (of $f)"; fail=1
  elif ! cmp -s "$f" "$m"; then echo "drift: $f != $m"; fail=1; fi
done
for f in .claude/agents/*.md; do
  [ -e "$f" ] || continue
  m=".agents/skills/$(basename "$f" .md)/SKILL.md"
  if [ ! -f "$m" ]; then echo "missing mirror: $m (of $f)"; fail=1
  elif ! diff -q <(body "$f") <(body "$m") >/dev/null; then echo "drift: body of $f != $m"; fail=1; fi
done
for m in .agents/skills/*/SKILL.md; do
  [ -e "$m" ] || continue
  n=$(basename "$(dirname "$m")")
  [ -f ".claude/skills/$n/SKILL.md" ] || [ -f ".claude/agents/$n.md" ] || { echo "orphan: $m has no .claude counterpart"; fail=1; }
done
[ "$fail" -eq 0 ] || { echo "Edit both copies together (see AGENTS.md)."; exit 1; }
