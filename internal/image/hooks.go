package image

// OnRemove calls f with the ID of each image Remove deletes, once the
// store has let go of it, until the returned function is called.
func (s *Store) OnRemove(f func(id string)) (cancel func()) {
	s.removedMu.Lock()
	defer s.removedMu.Unlock()
	if s.removed == nil {
		s.removed = map[int]func(string){}
	}
	n := s.nextHook
	s.nextHook++
	s.removed[n] = f
	return func() {
		s.removedMu.Lock()
		defer s.removedMu.Unlock()
		delete(s.removed, n)
	}
}

func (s *Store) notifyRemoved(id string) {
	s.removedMu.Lock()
	hooks := make([]func(string), 0, len(s.removed))
	for _, f := range s.removed {
		hooks = append(hooks, f)
	}
	s.removedMu.Unlock()
	for _, f := range hooks {
		f(id)
	}
}
