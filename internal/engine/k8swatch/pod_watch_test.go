package k8swatch

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tilt-dev/tilt/pkg/apis/core/v1alpha1"

	"github.com/tilt-dev/tilt/internal/store/k8sconv"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tilt-dev/tilt/internal/k8s"
	"github.com/tilt-dev/tilt/internal/k8s/testyaml"
	"github.com/tilt-dev/tilt/internal/store"
	"github.com/tilt-dev/tilt/internal/testutils"
	"github.com/tilt-dev/tilt/internal/testutils/manifestbuilder"
	"github.com/tilt-dev/tilt/internal/testutils/podbuilder"
	"github.com/tilt-dev/tilt/internal/testutils/tempdir"
	"github.com/tilt-dev/tilt/pkg/model"
)

func TestPodWatch(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	manifest := f.addManifestWithSelectors("server")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb := podbuilder.New(t, manifest)
	p := pb.Build()

	// Simulate the deployed entities in the engine state
	entities := pb.ObjectTreeEntities()
	f.addDeployedEntity(manifest, entities.Deployment())
	f.kClient.InjectEntityByName(entities...)

	f.kClient.EmitPod(labels.Everything(), p)

	f.assertObservedPods(p)
}

func TestPodWatchChangeEventBeforeUID(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	manifest := f.addManifestWithSelectors("server")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb := podbuilder.New(t, manifest)
	p := pb.Build()

	f.kClient.InjectEntityByName(pb.ObjectTreeEntities()...)
	f.kClient.EmitPod(labels.Everything(), p)
	f.assertObservedPods()

	// Simulate the deployed entities in the engine state after
	// the pod event.
	entities := pb.ObjectTreeEntities()
	f.addDeployedEntity(manifest, entities.Deployment())
	f.kClient.InjectEntityByName(entities...)

	f.assertObservedPods(p)
}

// We had a bug where if newPod.resourceVersion < oldPod.resourceVersion (using string comparison!)
// then we'd ignore the new pod. This meant, e.g., once we got an update for resourceVersion "9", we'd
// ignore updates for resourceVersions "10" through "89" and "100" through "899"
func TestPodWatchResourceVersionStringLessThan(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	manifest := f.addManifestWithSelectors("server")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb := podbuilder.New(t, manifest).WithResourceVersion("9")

	// Simulate the deployed entities in the engine state
	entities := pb.ObjectTreeEntities()
	f.addDeployedEntity(manifest, entities.Deployment())
	f.kClient.InjectEntityByName(entities...)

	p1 := pb.Build()
	f.kClient.EmitPod(labels.Everything(), p1)

	f.assertObservedPods(p1)

	p2 := pb.WithResourceVersion("10").Build()
	f.kClient.EmitPod(labels.Everything(), p2)

	f.assertObservedPods(p1, p2)
}

func TestPodWatchExtraSelectors(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	ls1 := labels.Set{"foo": "bar"}
	ls2 := labels.Set{"baz": "quu"}
	manifest := f.addManifestWithSelectors("server", ls1, ls2)

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	p := podbuilder.New(t, manifest).
		WithPodLabel("foo", "bar").
		WithUnknownOwner().
		Build()
	f.kClient.EmitPod(labels.Everything(), p)

	f.assertObservedPods(p)
	f.assertObservedManifests(manifest.Name)
}

func TestPodWatchHandleSelectorChange(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	ls1 := labels.Set{"foo": "bar"}
	manifest := f.addManifestWithSelectors("server1", ls1)

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	p := podbuilder.New(t, manifest).
		WithPodLabel("foo", "bar").
		WithUnknownOwner().
		Build()
	f.kClient.EmitPod(labels.Everything(), p)

	f.assertObservedPods(p)
	f.clearPods()

	ls2 := labels.Set{"baz": "quu"}
	manifest2 := f.addManifestWithSelectors("server2", ls2)
	f.removeManifest("server1")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb2 := podbuilder.New(t, manifest2).WithPodID("pod2")
	p2 := pb2.Build()
	p2Entities := pb2.ObjectTreeEntities()
	f.addDeployedEntity(manifest2, p2Entities.Deployment())
	f.kClient.InjectEntityByName(p2Entities...)
	f.kClient.EmitPod(labels.Everything(), p2)
	f.assertObservedPods(p2)
	f.clearPods()

	p3 := podbuilder.New(t, manifest2).
		WithPodID("pod3").
		WithPodLabel("foo", "bar").
		WithUnknownOwner().
		Build()
	f.kClient.EmitPod(labels.Everything(), p3)

	p4 := podbuilder.New(t, manifest2).
		WithPodID("pod4").
		WithPodLabel("baz", "quu").
		WithUnknownOwner().
		Build()
	f.kClient.EmitPod(labels.Everything(), p4)

	p5 := podbuilder.New(t, manifest2).
		WithPodID("pod5").
		Build()
	f.kClient.EmitPod(labels.Everything(), p5)

	f.assertObservedPods(p4, p5)
	assert.Equal(t, []model.ManifestName{manifest2.Name, manifest2.Name}, f.manifestNames)
}

func TestPodsDispatchedInOrder(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()
	manifest := f.addManifestWithSelectors("server")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb := podbuilder.New(t, manifest)

	entities := pb.ObjectTreeEntities()
	f.addDeployedEntity(manifest, entities.Deployment())
	f.kClient.InjectEntityByName(entities...)

	count := 20
	pods := []*v1.Pod{}
	for i := 0; i < count; i++ {
		v := strconv.Itoa(i)
		pod := pb.
			WithResourceVersion(v).
			WithTemplateSpecHash(k8s.PodTemplateSpecHash(v)).
			Build()
		pods = append(pods, pod)
	}

	for _, pod := range pods {
		f.kClient.EmitPod(labels.Everything(), pod)
	}

	f.waitForPodActionCount(count)

	// Make sure the pods showed up in order.
	for i := 1; i < count; i++ {
		pod := f.pods[i]
		lastPod := f.pods[i-1]
		podV, _ := strconv.Atoi(pod.PodTemplateSpecHash)
		lastPodV, _ := strconv.Atoi(lastPod.PodTemplateSpecHash)
		if lastPodV > podV {
			t.Fatalf("Pods appeared out of order\nPod %d: %v\nPod %d: %v\n", i-1, lastPod, i, pod)
		}
	}
}

func TestPodWatchReadd(t *testing.T) {
	f := newPWFixture(t)
	defer f.TearDown()

	manifest := f.addManifestWithSelectors("server")

	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	pb := podbuilder.New(t, manifest)
	p := pb.Build()
	entities := pb.ObjectTreeEntities()
	f.addDeployedEntity(manifest, entities.Deployment())
	f.kClient.InjectEntityByName(entities...)
	f.kClient.EmitPod(labels.Everything(), p)

	f.assertObservedPods(p)

	f.removeManifest("server")
	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	f.pods = nil
	_ = f.addManifestWithSelectors("server")
	f.addDeployedEntity(manifest, pb.ObjectTreeEntities().Deployment())
	f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	// Make sure the pods are re-broadcast.
	// Even though the pod didn't change when the manifest was
	// redeployed, we still need to broadcast the pod to make
	// sure it gets repopulated.
	f.assertObservedPods(p)
}

func (f *pwFixture) addManifestWithSelectors(manifestName string, ls ...labels.Set) model.Manifest {
	state := f.store.LockMutableStateForTesting()
	m := manifestbuilder.New(f, model.ManifestName(manifestName)).
		WithK8sYAML(testyaml.SanchoYAML).
		WithK8sPodSelectors(ls).
		Build()
	mt := store.NewManifestTarget(m)
	state.UpsertManifestTarget(mt)
	f.store.UnlockMutableState()
	return mt.Manifest
}

func (f *pwFixture) removeManifest(mn model.ManifestName) {
	state := f.store.LockMutableStateForTesting()
	state.RemoveManifestTarget(mn)
	f.store.UnlockMutableState()
}

type pwFixture struct {
	*tempdir.TempDirFixture
	t             *testing.T
	kClient       *k8s.FakeK8sClient
	pw            *PodWatcher
	ctx           context.Context
	cancel        func()
	store         *store.Store
	pods          []*v1alpha1.Pod
	manifestNames []model.ManifestName
	mu            sync.Mutex
}

func (pw *pwFixture) reducer(ctx context.Context, state *store.EngineState, action store.Action) {
	pw.mu.Lock()
	defer pw.mu.Unlock()

	a, ok := action.(PodChangeAction)
	if !ok {
		pw.t.Errorf("Expected action type PodLogAction. Actual: %T", action)
	}
	pw.pods = append(pw.pods, a.Pod)
	pw.manifestNames = append(pw.manifestNames, a.ManifestName)
}

func newPWFixture(t *testing.T) *pwFixture {
	kClient := k8s.NewFakeK8sClient()

	ctx, _, _ := testutils.CtxAndAnalyticsForTest()
	ctx, cancel := context.WithCancel(ctx)

	of := k8s.ProvideOwnerFetcher(ctx, kClient)
	pw := NewPodWatcher(kClient, of, k8s.DefaultNamespace)
	ret := &pwFixture{
		TempDirFixture: tempdir.NewTempDirFixture(t),
		kClient:        kClient,
		pw:             pw,
		ctx:            ctx,
		cancel:         cancel,
		t:              t,
	}

	st := store.NewStore(store.Reducer(ret.reducer), store.LogActionsFlag(false))
	go func() {
		err := st.Loop(ctx)
		testutils.FailOnNonCanceledErr(t, err, "store.Loop failed")
	}()

	ret.store = st

	return ret
}

func (f *pwFixture) TearDown() {
	f.TempDirFixture.TearDown()
	f.kClient.TearDown()
	f.cancel()
}

func (f *pwFixture) addDeployedEntity(m model.Manifest, entity k8s.K8sEntity) {
	defer f.pw.OnChange(f.ctx, f.store, store.LegacyChangeSummary())

	state := f.store.LockMutableStateForTesting()
	defer f.store.UnlockMutableState()
	mState, ok := state.ManifestState(m.Name)
	if !ok {
		f.t.Fatalf("Unknown manifest: %s", m.Name)
	}

	runtimeState := mState.K8sRuntimeState()
	runtimeState.DeployedEntities = k8s.ObjRefList{entity.ToObjectReference()}
	mState.RuntimeState = runtimeState
}

func (f *pwFixture) waitForPodActionCount(count int) {
	f.t.Helper()
	start := time.Now()
	for time.Since(start) < time.Second {
		f.mu.Lock()
		podCount := len(f.pods)
		f.mu.Unlock()

		if podCount >= count {
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	f.t.Fatalf("Timeout waiting for %d pod actions", count)
}

func (f *pwFixture) assertObservedPods(pods ...*corev1.Pod) {
	f.t.Helper()
	f.waitForPodActionCount(len(pods))
	var toCmp []*v1alpha1.Pod
	for _, p := range pods {
		toCmp = append(toCmp, k8sconv.Pod(f.ctx, p))
	}
	require.ElementsMatch(f.t, toCmp, f.pods)
}

func (f *pwFixture) assertObservedManifests(manifests ...model.ManifestName) {
	start := time.Now()
	for time.Since(start) < time.Second {
		if len(manifests) == len(f.manifestNames) {
			break
		}
	}

	require.ElementsMatch(f.t, manifests, f.manifestNames)
}

func (f *pwFixture) clearPods() {
	f.pods = nil
	f.manifestNames = nil
}
