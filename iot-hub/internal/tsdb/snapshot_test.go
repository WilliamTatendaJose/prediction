package tsdb

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// A snapshot taken while the writer keeps writing is a consistent,
// openable database, and the writes are not blocked by it.
func TestSnapshotWhileWriting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := Open(ctx, "sqlite:"+filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := time.Now().Add(-time.Hour).UnixMilli()
	pts := make([]Point, 0, 20_000)
	for i := range 20_000 {
		pts = append(pts, Point{Sensor: "s", Field: "v", TS: base + int64(i), Value: float64(i)})
	}
	if err := db.WritePoints(ctx, pts); err != nil {
		t.Fatal(err)
	}

	var written atomic.Int64
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { // keeps writing during the snapshot
		ts := base + 1_000_000
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := db.WritePoints(ctx, []Point{{Sensor: "s", Field: "v", TS: ts, Value: 1}}); err != nil {
				done <- err
				return
			}
			ts++
			written.Add(1)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	before := written.Load()
	snap := filepath.Join(dir, "snap.db")
	if err := db.(Snapshotter).Snapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	during := written.Load() - before
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("writer failed during snapshot: %v", err)
	}
	if during == 0 {
		t.Error("no writes completed while the snapshot ran")
	}

	cp, err := Open(ctx, "sqlite:"+snap)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	var ok string
	if err := cp.(*sqlDB).db.QueryRow("PRAGMA integrity_check").Scan(&ok); err != nil || ok != "ok" {
		t.Fatalf("integrity %q %v", ok, err)
	}
	ts, _, err := cp.Raw(ctx, "s", "v", base, base+20_000, 100_000)
	if err != nil || len(ts) != 20_000 {
		t.Fatalf("snapshot has %d of the 20000 readings (%v)", len(ts), err)
	}
	if err := db.(Snapshotter).Snapshot(ctx, snap); err == nil {
		t.Error("snapshot over an existing file should fail")
	}
	t.Logf("%d writes completed during the snapshot", during)
}

func TestSnapshotPostgresRefused(t *testing.T) {
	s := &sqlDB{pg: true}
	if s.CanSnapshot() || !errors.Is(s.Snapshot(context.Background(), "x"), ErrNoSnapshot) {
		t.Fatal("postgres must refuse snapshots")
	}
}
