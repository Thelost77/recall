package memory

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const SchemaVersion = 1

const (
	ScopeProject = "project"
	ScopeGlobal  = "global"
)

var idPattern = regexp.MustCompile(`^m_[a-z0-9]{6,64}$`)

type Revision struct {
	Revision  int       `json:"revision"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

type Record struct {
	SchemaVersion   int        `json:"schemaVersion"`
	ID              string     `json:"id"`
	Scope           string     `json:"scope"`
	ProjectKey      string     `json:"projectKey,omitempty"`
	ProjectRemote   string     `json:"projectRemote,omitempty"`
	ProjectName     string     `json:"projectName,omitempty"`
	Source          string     `json:"source,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	ArchivedAt      *time.Time `json:"archivedAt"`
	CurrentRevision int        `json:"currentRevision"`
	Revisions       []Revision `json:"revisions"`
}

func (r Record) CurrentContent() string {
	if len(r.Revisions) == 0 || r.CurrentRevision != r.Revisions[len(r.Revisions)-1].Revision {
		return ""
	}
	return r.Revisions[len(r.Revisions)-1].Content
}

func (r Record) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %d", r.SchemaVersion)
	}
	if !idPattern.MatchString(r.ID) {
		return fmt.Errorf("invalid memory ID %q", r.ID)
	}
	if r.Scope != ScopeProject && r.Scope != ScopeGlobal {
		return fmt.Errorf("invalid scope %q", r.Scope)
	}
	if r.Scope == ScopeProject {
		if strings.TrimSpace(r.ProjectKey) == "" || strings.TrimSpace(r.ProjectName) == "" {
			return fmt.Errorf("project memory requires projectKey and projectName")
		}
	} else if r.ProjectKey != "" || r.ProjectRemote != "" || r.ProjectName != "" {
		return fmt.Errorf("global memory must not contain project metadata")
	}
	if r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return fmt.Errorf("createdAt and updatedAt are required")
	}
	if r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("updatedAt precedes createdAt")
	}
	if r.ArchivedAt != nil && r.ArchivedAt.Before(r.CreatedAt) {
		return fmt.Errorf("archivedAt precedes createdAt")
	}
	if len(r.Revisions) == 0 || r.CurrentRevision != len(r.Revisions) {
		return fmt.Errorf("currentRevision must identify the last revision")
	}
	for index, revision := range r.Revisions {
		if revision.Revision != index+1 {
			return fmt.Errorf("revision numbers must be contiguous")
		}
		if strings.TrimSpace(revision.Content) == "" {
			return fmt.Errorf("revision %d content is empty", revision.Revision)
		}
		if revision.CreatedAt.IsZero() || revision.CreatedAt.Before(r.CreatedAt) || revision.CreatedAt.After(r.UpdatedAt) {
			return fmt.Errorf("revision %d has an invalid createdAt", revision.Revision)
		}
	}
	return nil
}

type Warning struct {
	Type          string   `json:"type"`
	Message       string   `json:"message"`
	Path          string   `json:"path,omitempty"`
	MemoryID      string   `json:"memoryId,omitempty"`
	ConflictPaths []string `json:"conflictPaths,omitempty"`
}
