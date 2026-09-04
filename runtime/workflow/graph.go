// Package workflow validates durable execution graphs independently of the host's
// storage, coding adapters and machine policy.
package workflow

import "fmt"

type Node struct {
	ID          string   `json:"id"`
	DependsOn   []string `json:"depends_on,omitempty"`
	RetryFrom   string   `json:"retry_from,omitempty"`
	MaxAttempts int      `json:"max_attempts,omitempty"`
}

func Validate(nodes []Node) error {
	if len(nodes) == 0 || len(nodes) > 100 {
		return fmt.Errorf("workflow needs 1..100 stages")
	}
	byID := map[string]Node{}
	for _, n := range nodes {
		if n.ID == "" || byID[n.ID].ID != "" {
			return fmt.Errorf("stage IDs must be nonempty and unique")
		}
		if n.MaxAttempts < 1 || n.MaxAttempts > 100 {
			return fmt.Errorf("stage %s needs max_attempts in 1..100", n.ID)
		}
		byID[n.ID] = n
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if visited[id] {
			return nil
		}
		n, ok := byID[id]
		if !ok {
			return fmt.Errorf("unknown dependency %s", id)
		}
		visiting[id] = true
		for _, d := range n.DependsOn {
			if err := visit(d); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for _, n := range nodes {
		if err := visit(n.ID); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		if n.RetryFrom != "" && !Descendants(nodes, n.RetryFrom)[n.ID] {
			return fmt.Errorf("retry_from for %s must name itself or an ancestor", n.ID)
		}
	}
	return nil
}

// Descendants includes root and all stages invalidated by rerunning root.
func Descendants(nodes []Node, root string) map[string]bool {
	found := map[string]bool{}
	for _, n := range nodes {
		if n.ID == root {
			found[root] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, n := range nodes {
			if !found[n.ID] {
				for _, d := range n.DependsOn {
					if found[d] {
						found[n.ID] = true
						changed = true
						break
					}
				}
			}
		}
	}
	return found
}
