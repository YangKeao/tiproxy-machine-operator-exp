package portallocator

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/types"
)

type LinkPortRequest struct {
	UID types.UID
	// Name is used in error messages (prefer "namespace/name").
	Name string
	// RequestedPort pins this link to a specific port when set.
	RequestedPort *int32
}

// ReconcilePorts keeps existing valid allocations, honors requested ports, and allocates ports for new links.
func ReconcilePorts(existing map[string]int32, linkRequests []LinkPortRequest, start, end int32) (map[string]int32, error) {
	if start <= 0 {
		return nil, fmt.Errorf("invalid port range start: %d", start)
	}
	if end < start {
		return nil, fmt.Errorf("invalid port range: %d-%d", start, end)
	}

	requests := make([]LinkPortRequest, 0, len(linkRequests))
	want := make(map[string]struct{}, len(linkRequests))
	for _, req := range linkRequests {
		s := string(req.UID)
		if _, ok := want[s]; ok {
			continue
		}
		if req.Name == "" {
			req.Name = s
		}
		want[s] = struct{}{}
		requests = append(requests, req)
	}
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].Name == requests[j].Name {
			return string(requests[i].UID) < string(requests[j].UID)
		}
		return requests[i].Name < requests[j].Name
	})

	result := make(map[string]int32, len(requests))
	used := make(map[int32]struct{}, len(requests))
	requestedByPort := make(map[int32]LinkPortRequest, len(requests))

	for _, req := range requests {
		if req.RequestedPort == nil {
			continue
		}
		port := *req.RequestedPort
		if port < start || port > end {
			return nil, fmt.Errorf("link %s requests port %d outside range %d-%d", req.Name, port, start, end)
		}
		if prev, ok := requestedByPort[port]; ok {
			return nil, fmt.Errorf("requested port %d conflicts between %s and %s", port, prev.Name, req.Name)
		}
		requestedByPort[port] = req
		result[string(req.UID)] = port
		used[port] = struct{}{}
	}

	for _, req := range requests {
		uid := string(req.UID)
		if _, pinned := result[uid]; pinned {
			continue
		}
		if existing == nil {
			continue
		}
		port, ok := existing[uid]
		if !ok {
			continue
		}
		if port < start || port > end {
			continue
		}
		if _, taken := used[port]; taken {
			continue
		}
		result[uid] = port
		used[port] = struct{}{}
	}

	next := start
	findNext := func() (int32, bool) {
		for next <= end {
			if _, ok := used[next]; !ok {
				p := next
				next++
				return p, true
			}
			next++
		}
		return 0, false
	}

	for _, req := range requests {
		uid := string(req.UID)
		if _, ok := result[uid]; ok {
			continue
		}
		port, ok := findNext()
		if !ok {
			return nil, fmt.Errorf("insufficient ports in range %d-%d for %d links", start, end, len(requests))
		}
		result[uid] = port
		used[port] = struct{}{}
	}

	return result, nil
}
