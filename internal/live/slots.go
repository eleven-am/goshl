package live

func (s *Session) reserve(p *prepared) error {
	if p.params.CopyVideo {
		return nil
	}
	s.mu.Lock()
	held := s.slot
	s.mu.Unlock()
	if held {
		return nil
	}
	if !s.manager.config.Slots.TryAcquireLive() {
		return ErrCapacity
	}
	p.slot = true
	return nil
}

func (s *Session) discard(p *prepared) {
	p.close()
	if p.slot {
		s.manager.config.Slots.ReleaseLive()
		p.slot = false
	}
}

func (s *Session) adopt(p *prepared) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := s.manager.config.Slots
	switch {
	case p.params.CopyVideo:
		if p.slot {
			slots.ReleaseLive()
		}
		if s.slot {
			slots.ReleaseLive()
			s.slot = false
		}
	case p.slot && s.slot:
		slots.ReleaseLive()
	case p.slot:
		s.slot = true
	case !s.slot:
		if !slots.TryAcquireLive() {
			return ErrCapacity
		}
		s.slot = true
	}
	p.slot = false
	s.info = p.info
	s.label = p.label
	s.epochs++
	return nil
}

func (s *Session) releaseSlot() {
	if s.slot {
		s.manager.config.Slots.ReleaseLive()
		s.slot = false
	}
}
