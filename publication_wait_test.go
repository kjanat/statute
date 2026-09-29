package statute

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestAwaitPublishedTableIgnoresUnrelatedGenerations(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &dockerProvider{srv: &server{}, generationChanged: make(chan struct{})}
		initial, unrelated, settled := &dynamicTable{}, &dynamicTable{}, &dynamicTable{}
		publishTestTable(t, p, initial)
		checked := make(chan *dynamicTable)
		proceed := make(chan struct{})
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- awaitPublishedTable(ctx, p, func(table *dynamicTable) bool {
				if table == settled {
					return true
				}
				select {
				case checked <- table:
				case <-ctx.Done():
					return false
				}
				select {
				case <-proceed:
				case <-ctx.Done():
				}
				return false
			})
		}()
		if table := <-checked; table != initial {
			t.Fatal("initial generation was not checked")
		}
		publishTestTable(t, p, unrelated)
		proceed <- struct{}{}
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("unrelated publication completed wait: %v", err)
		default:
		}
		if table := <-checked; table != unrelated {
			t.Fatal("unrelated generation was not checked")
		}
		// A publication during predicate evaluation must survive until the wait.
		publishTestTable(t, p, settled)
		proceed <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("settled publication: %v", err)
		}
	})
}

func publishTestTable(t *testing.T, p *dockerProvider, table *dynamicTable) {
	t.Helper()
	if !p.publishGeneration(table, dockerGenerationVersions{}) {
		t.Fatal("publication rejected")
	}
}

func TestAwaitPublishedTableAlreadySatisfied(t *testing.T) {
	p := &dockerProvider{srv: &server{}, generationChanged: make(chan struct{})}
	table := &dynamicTable{}
	p.srv.dynamic.Store(table)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := awaitPublishedTable(ctx, p, func(got *dynamicTable) bool { return got == table }); err != nil {
		t.Fatalf("already published table required another notification: %v", err)
	}
}

func TestAwaitPublishedTableDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &dockerProvider{srv: &server{}, generationChanged: make(chan struct{})}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := awaitPublishedTable(ctx, p, func(table *dynamicTable) bool { return table != nil })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("missing publication: %v, want deadline exceeded", err)
		}
	})
}
