package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sample(id string) SessionMeta {
	return SessionMeta{
		ID: id, Name: "shell-" + id, CreatedAt: 1000, LastActivityAt: 1000,
		Cols: 80, Rows: 24, Status: StatusRunning,
	}
}

func TestUpsertAndList(t *testing.T) {
	st := openTemp(t)
	if err := st.Upsert(sample("a")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.Upsert(sample("b")); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 rows, got %d", len(list))
	}
	if list[0].ID != "a" || list[0].Name != "shell-a" || list[0].Status != StatusRunning {
		t.Fatalf("unexpected row: %+v", list[0])
	}
}

func TestUpsertPreservesCreatedAt(t *testing.T) {
	st := openTemp(t)
	m := sample("a")
	st.Upsert(m)
	m2 := m
	m2.CreatedAt = 9999 // should be ignored on conflict
	m2.Name = "renamed"
	st.Upsert(m2)

	list, _ := st.List()
	if list[0].CreatedAt != 1000 {
		t.Fatalf("created_at must be preserved, got %d", list[0].CreatedAt)
	}
	if list[0].Name != "renamed" {
		t.Fatalf("name should update, got %q", list[0].Name)
	}
}

func TestFieldMutators(t *testing.T) {
	st := openTemp(t)
	st.Upsert(sample("a"))

	if err := st.SetName("a", "newname"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetActivity("a", 2000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetClientDisconnect("a", 2500); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSize("a", 120, 40); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus("a", StatusDead); err != nil {
		t.Fatal(err)
	}

	got := st.mustGet(t, "a")
	if got.Name != "newname" || got.LastActivityAt != 2000 ||
		got.LastClientDisconnectAt != 2500 || got.Cols != 120 || got.Rows != 40 ||
		got.Status != StatusDead {
		t.Fatalf("mutators did not all apply: %+v", got)
	}
}

func TestMarkAllDetached(t *testing.T) {
	st := openTemp(t)
	st.Upsert(sample("a")) // running
	dead := sample("b")
	dead.Status = StatusDead
	st.Upsert(dead)

	if err := st.MarkAllDetached(); err != nil {
		t.Fatal(err)
	}
	if st.mustGet(t, "a").Status != StatusDetached {
		t.Fatal("running should become detached")
	}
	if st.mustGet(t, "b").Status != StatusDead {
		t.Fatal("dead must stay dead")
	}
}

func TestDelete(t *testing.T) {
	st := openTemp(t)
	st.Upsert(sample("a"))
	if err := st.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.List(); len(list) != 0 {
		t.Fatalf("expected empty after delete, got %d", len(list))
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Upsert(sample("a"))
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	list, _ := st2.List()
	if len(list) != 1 || list[0].ID != "a" {
		t.Fatalf("data did not persist across reopen: %+v", list)
	}
}

// mustGet is a test helper that fetches one row by id via List.
func (s *Store) mustGet(t *testing.T, id string) SessionMeta {
	t.Helper()
	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, m := range list {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("row %q not found", id)
	return SessionMeta{}
}
