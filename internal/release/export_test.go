package release

import "testing"

// These adapters let external tests exercise control with the actual signed
// store fixtures without introducing a production release -> control dependency.
func NewFixtureStore(t *testing.T) *Store          { return newTestStore(t) }
func InitializeFixture(t *testing.T, s *Store) Ref { return initializeFixture(t, s) }
func StageFixture(t *testing.T, s *Store, id string, sequence uint64) Ref {
	return stageFixture(t, s, id, sequence)
}
