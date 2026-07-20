package store

import "fmt"

// validateDAG checks the whole plan DAG once at save time (Kahn topo-sort).
// It rejects: duplicate task IDs, self-dependencies, duplicate edges,
// edges pointing outside the plan (cross-plan/unknown tasks), and cycles.
func validateDAG(tasks []NewTask) error {
	ids := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		if t.ID == "" {
			return fmt.Errorf("dag: task with empty id")
		}
		if ids[t.ID] {
			return fmt.Errorf("dag: duplicate task id %q", t.ID)
		}
		ids[t.ID] = true
	}

	indegree := make(map[string]int, len(tasks))
	dependents := make(map[string][]string, len(tasks)) // dep -> tasks that depend on it
	for _, t := range tasks {
		seen := make(map[string]bool, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			if dep == t.ID {
				return fmt.Errorf("dag: task %q depends on itself", t.ID)
			}
			if !ids[dep] {
				return fmt.Errorf("dag: task %q depends on %q which is not in this plan", t.ID, dep)
			}
			if seen[dep] {
				return fmt.Errorf("dag: duplicate edge %q -> %q", t.ID, dep)
			}
			seen[dep] = true
			indegree[t.ID]++
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	queue := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if indegree[t.ID] == 0 {
			queue = append(queue, t.ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, next := range dependents[id] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if visited != len(tasks) {
		return fmt.Errorf("dag: cycle detected in plan dependencies")
	}
	return nil
}
