#!/usr/bin/env bash
set -euo pipefail
go test ./cmd/pig -run '^TestSkillMetadataUsesFilesWithoutChangingConfigBundlePaths$' -count=1 -v | grep '^SKILL_METADATA '
