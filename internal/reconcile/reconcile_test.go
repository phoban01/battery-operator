/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package reconcile

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/phoban01/battery-operator/internal/clock"
)

// scope is a controller's own scope in the tests: a Scope of a Pod with a
// field of its own.
type scope struct {
	*Scope[*corev1.Pod]
	ran []string
}

// step is a subreconciler that records its name and returns r and err.
func step(name string, r Result, err error) Func[*scope] {
	return func(_ context.Context, s *scope) (Result, error) {
		s.ran = append(s.ran, name)
		return r, err
	}
}

func newScope(t *testing.T, pod *corev1.Pod) *scope {
	t.Helper()
	return &scope{Scope: NewScope(pod, nil, logr.Discard(), clock.NewFake(time.Unix(1000, 0)))}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "p", Generation: 3}}
}

func TestResultMerge(t *testing.T) {
	cases := []struct {
		name    string
		a, b    Result
		want    Result
		wantCtl ctrl.Result
	}{
		{"nothing", Result{}, Result{}, Result{}, ctrl.Result{}},
		{"stop wins", Result{}, Result{Stop: true}, Result{Stop: true}, ctrl.Result{}},
		{"first requeue", Result{RequeueAfter: time.Minute}, Result{}, Result{RequeueAfter: time.Minute},
			ctrl.Result{RequeueAfter: time.Minute}},
		{"second requeue", Result{}, Result{RequeueAfter: time.Minute}, Result{RequeueAfter: time.Minute},
			ctrl.Result{RequeueAfter: time.Minute}},
		{"shortest requeue", Result{RequeueAfter: time.Minute}, Result{RequeueAfter: time.Second},
			Result{RequeueAfter: time.Second}, ctrl.Result{RequeueAfter: time.Second}},
		{"longer requeue loses", Result{RequeueAfter: time.Second}, Result{RequeueAfter: time.Minute},
			Result{RequeueAfter: time.Second}, ctrl.Result{RequeueAfter: time.Second}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.a.Merge(tc.b)
			if got != tc.want {
				t.Errorf("Merge = %+v, want %+v", got, tc.want)
			}
			if got.Ctrl() != tc.wantCtl {
				t.Errorf("Ctrl = %+v, want %+v", got.Ctrl(), tc.wantCtl)
			}
		})
	}
}

func TestChainRunsStepsUntilOneStops(t *testing.T) {
	s := newScope(t, testPod())
	c := Chain[*scope]{
		Steps: []SubReconciler[*scope]{
			step("a", Result{RequeueAfter: time.Minute}, nil),
			step("b", Result{Stop: true, RequeueAfter: time.Second}, nil),
			step("c", Result{}, nil),
		},
		Finally: []SubReconciler[*scope]{
			step("f1", Result{Stop: true, RequeueAfter: time.Hour}, nil),
			step("f2", Result{}, nil),
		},
	}
	res, err := c.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b", "f1", "f2"}; !slices.Equal(s.ran, want) {
		t.Errorf("ran %v, want %v", s.ran, want)
	}
	if want := (Result{Stop: true, RequeueAfter: time.Second}); res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}
}

func TestChainStopsAtAnErrorAndStillRunsFinally(t *testing.T) {
	s := newScope(t, testPod())
	stepErr, finalErr := errors.New("step"), errors.New("finally")
	c := Chain[*scope]{
		Steps:   []SubReconciler[*scope]{step("a", Result{}, stepErr), step("b", Result{}, nil)},
		Finally: []SubReconciler[*scope]{step("f1", Result{}, finalErr), step("f2", Result{}, nil)},
	}
	_, err := c.Run(context.Background(), s)
	if !errors.Is(err, stepErr) || !errors.Is(err, finalErr) {
		t.Errorf("error %v, want both %v and %v", err, stepErr, finalErr)
	}
	if want := []string{"a", "f1", "f2"}; !slices.Equal(s.ran, want) {
		t.Errorf("ran %v, want %v", s.ran, want)
	}
}

func TestStepsAndChainAsAStep(t *testing.T) {
	s := newScope(t, testPod())
	inner := Steps(step("i1", Result{Stop: true}, nil), step("i2", Result{}, nil))
	res, err := Steps[*scope](inner, step("after", Result{}, nil)).Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	// A Chain as a step passes its Stop on.
	if want := []string{"i1"}; !slices.Equal(s.ran, want) || !res.Stop {
		t.Errorf("ran %v with %+v, want %v and a stop", s.ran, res, want)
	}
}

func TestGroupKeepsItsStopToItself(t *testing.T) {
	s := newScope(t, testPod())
	checks := Group[*scope]{Steps(
		step("check1", Result{}, nil),
		step("check2", Result{Stop: true, RequeueAfter: time.Minute}, nil),
		step("check3", Result{}, nil),
	)}
	res, err := Steps[*scope](checks, step("after", Result{}, nil)).Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"check1", "check2", "after"}; !slices.Equal(s.ran, want) {
		t.Errorf("ran %v, want %v", s.ran, want)
	}
	if want := (Result{RequeueAfter: time.Minute}); res != want {
		t.Errorf("result %+v, want %+v", res, want)
	}

	s.ran = nil
	boom := errors.New("boom")
	failing := Group[*scope]{Steps(step("check", Result{}, boom))}
	if _, err := Steps[*scope](failing, step("after", Result{}, nil)).Run(context.Background(), s); !errors.Is(err, boom) {
		t.Errorf("error %v, want %v", err, boom)
	}
	if want := []string{"check"}; !slices.Equal(s.ran, want) {
		t.Errorf("ran %v, want %v", s.ran, want)
	}
}

func TestNewScopeKeepsTheObjectAsFetched(t *testing.T) {
	pod := testPod()
	s := NewScope(pod, nil, logr.Discard(), nil)
	if _, ok := s.Clock.(clock.Real); !ok {
		t.Errorf("clock %T, want clock.Real", s.Clock)
	}
	s.Object.Labels = map[string]string{"changed": "true"}
	if s.Original.Labels != nil {
		t.Error("changing Object changed Original")
	}
	if s.Base() != s {
		t.Error("Base is not the scope itself")
	}
}

func TestEnsureFinalizerStopsOnlyWhenItAddsOne(t *testing.T) {
	const name = "example.test/finalizer"
	s := newScope(t, testPod())
	f := EnsureFinalizer[*scope, *corev1.Pod]{Name: name}

	res, err := f.Reconcile(context.Background(), s)
	if err != nil || !res.Stop {
		t.Fatalf("first Reconcile = %+v, %v; want a stop", res, err)
	}
	if want := []string{name}; !slices.Equal(s.Object.Finalizers, want) {
		t.Errorf("finalizers %v, want %v", s.Object.Finalizers, want)
	}
	res, err = f.Reconcile(context.Background(), s)
	if err != nil || res.Stop {
		t.Fatalf("second Reconcile = %+v, %v; want no stop", res, err)
	}
	if want := []string{name}; !slices.Equal(s.Object.Finalizers, want) {
		t.Errorf("finalizers %v, want %v", s.Object.Finalizers, want)
	}
}

func TestSetConditionStampsGenerationAndClock(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0))
	pod := testPod()
	var conds []metav1.Condition
	if !SetCondition(&conds, pod, clk, "Ready", metav1.ConditionTrue, "Fine", "All fine") {
		t.Fatal("SetCondition reported no change")
	}
	c := meta.FindStatusCondition(conds, "Ready")
	if c == nil || c.ObservedGeneration != 3 || !c.LastTransitionTime.Time.Equal(time.Unix(1000, 0)) ||
		c.Reason != "Fine" || c.Message != "All fine" || c.Status != metav1.ConditionTrue {
		t.Fatalf("condition %+v", c)
	}
	if SetCondition(&conds, pod, clk, "Ready", metav1.ConditionTrue, "Fine", "All fine") {
		t.Error("SetCondition reported a change for the same condition")
	}
}

// patches counts the patches the fake client receives.
type patches struct{ status, object int }

func fakeClient(t *testing.T, p *patches, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				p.object++
				return c.Patch(ctx, obj, patch, opts...)
			},
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch,
				opts ...client.SubResourcePatchOption) error {
				p.status++
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
}

func TestPatchWritesWhatChanged(t *testing.T) {
	ctx := context.Background()
	var p patches
	c := fakeClient(t, &p, testPod())
	fetched := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(testPod()), fetched); err != nil {
		t.Fatal(err)
	}

	s := NewScope(fetched, c, logr.Discard(), nil)
	if err := s.Patch(ctx); err != nil {
		t.Fatal(err)
	}
	if p != (patches{}) {
		t.Errorf("patches %+v for an unchanged object, want none", p)
	}

	s.Object.Status.Phase = corev1.PodRunning
	if err := s.Patch(ctx); err != nil {
		t.Fatal(err)
	}
	if p != (patches{status: 1}) {
		t.Errorf("patches %+v for a status change, want one of status", p)
	}

	s = NewScope(s.Object, c, logr.Discard(), nil)
	s.Object.Labels = map[string]string{"a": "b"}
	if err := s.Patch(ctx); err != nil {
		t.Fatal(err)
	}
	if p != (patches{status: 1, object: 1}) {
		t.Errorf("patches %+v after a metadata change, want one more of the object", p)
	}

	stored := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(testPod()), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != corev1.PodRunning || stored.Labels["a"] != "b" {
		t.Errorf("stored %+v, want the phase and the label written", stored)
	}
}

func TestPatchIgnoresAnObjectAlreadyGone(t *testing.T) {
	var p patches
	c := fakeClient(t, &p)
	s := NewScope(testPod(), c, logr.Discard(), nil)
	s.Object.Status.Phase = corev1.PodRunning
	s.Object.Labels = map[string]string{"a": "b"}
	if err := s.Patch(context.Background()); err != nil {
		t.Errorf("Patch of a missing object = %v, want nil", err)
	}
	if p != (patches{status: 1, object: 1}) {
		t.Errorf("patches %+v, want both tried", p)
	}
}
