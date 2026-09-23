// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package remote

// Peak is the most bytes one sub-stream has held on this side of the link,
// which the window bounds.
func (l *Link) Peak() int64 { return l.peak.Load() }

// Credited reports whether the connection agreed credit.
func (l *Link) Credited() bool { return l.peerWindow.Load() > 0 }

// Peak is the most bytes one sub-stream has held on the worker's side.
func (s *Server) Peak() int64 { return s.link.Peak() }

// Credited reports whether the worker's side of the stream agreed credit.
func (s *Server) Credited() bool { return s.link.Credited() }

// Running is how many operations the worker is executing.
func (s *Server) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.running)
}

// Peak is the most bytes one sub-stream has held on the control plane's side
// of any stream the environment's workers hold open.
func (h *Hub) Peak(environmentID string) int64 {
	var peak int64
	for _, link := range h.links(environmentID) {
		peak = max(peak, link.Peak())
	}
	return peak
}

// Credited reports whether every stream the environment's workers hold open
// agreed credit, and false where none is open.
func (h *Hub) Credited(environmentID string) bool {
	links := h.links(environmentID)
	for _, link := range links {
		if !link.Credited() {
			return false
		}
	}
	return len(links) > 0
}

func (h *Hub) links(environmentID string) []*Link {
	h.mu.Lock()
	defer h.mu.Unlock()
	env, held := h.environments[environmentID]
	if !held {
		return nil
	}
	var links []*Link
	for _, w := range env.workers {
		if w.link != nil {
			links = append(links, w.link)
		}
	}
	return links
}
