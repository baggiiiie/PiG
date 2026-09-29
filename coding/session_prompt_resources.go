// Ports packages/coding-agent/src/core/agent-session.ts.

package coding

import (
	"slices"

	icodingagent "github.com/MichaelKinsy/PiG/internal/codingagent"
)

// PromptTemplate is a resolved file-based prompt template.
type PromptTemplate = icodingagent.PromptTemplate

// Skill is a resolved skill supplied by the resource owner.
type Skill = icodingagent.SkillDef

type sessionPromptResources struct {
	templates []PromptTemplate
	skills    []*Skill
}

// SetPromptResources binds the resource owner's current resolved collections. It does not discover resources or change their configuration.
func (s *Session) SetPromptResources(templates []PromptTemplate, skills []*Skill) {
	s.promptResources.Store(&sessionPromptResources{templates: slices.Clone(templates), skills: slices.Clone(skills)})
}

func (s *Session) expandPromptText(text string) string {
	resources := s.promptResources.Load()
	if resources == nil {
		return text
	}
	if expanded, ok, err := icodingagent.ExpandSkillCommand(text, resources.skills); ok {
		text = expanded
	} else if err != nil {
		if runner := s.currentRunner(); runner != nil {
			runner.EmitError(err)
		}
	}
	if expanded, ok := icodingagent.ExpandPromptTemplate(text, resources.templates); ok {
		text = expanded
	}
	return text
}
