// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/kube"
	"github.com/bitwise-media-group/patchy/internal/rbactest"
	"github.com/bitwise-media-group/patchy/internal/web"
	"github.com/bitwise-media-group/patchy/internal/web/credstore"
)

// The user-credentials policies' refusals, each distinct so a case cannot
// pass on the wrong rule.
const (
	credRecordDenied       = "may be written only by the status server, for the"
	credStatusWriterDenied = "is written only by the egress broker"
	credFingerprintDenied  = "may not change the fingerprint"
	credStatusResetDenied  = "may only reset the health"
	credSecretWriterDenied = "may be written only by the status server or by a"
	credSecretNameDenied   = "must be named cred-<owner hash>-<provider>"
	credSecretDataDenied   = "must be Opaque and hold exactly one key"
)

const (
	credNamespace = v1alpha1.DefaultCredentialsNamespace
	saStatus      = "system:serviceaccount:patchy:patchy-status-server"
	saBroker      = "system:serviceaccount:patchy:patchy-egress-broker"
	// credUser holds a broad grant (every verb on the records and their
	// status, Secrets in the credentials namespace) but is no admin.
	credUser = "carol@example.com"
	// credAdmin is bound to the chart's -credentials-admin ClusterRole,
	// plus the same Secret grant.
	credAdmin = "dana@example.com"
)

// userCredentialArgs renders the chart with per-user credentials enforced.
var userCredentialArgs = []string{
	"--set", "attribution.mode=attribute",
	"--set", "egressBroker.userCredentials.mode=enforce",
}

// TestUserCredentialPoliciesAgainstAPIServer applies the chart rendered with
// per-user credentials on to a real API server, then drives each policy as
// the identities it judges: records and Secrets are written only by the
// status server or a credentials admin, never by a broad grant alone; each
// Secret carries its derived name, the matching owner label and one bounded
// key; status keeps one writer per field; and the chart's RBAC gives the
// status server Secret writes it can never read back, and the broker the
// reads the design scopes.
func TestUserCredentialPoliciesAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := t.Context()
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	seen := applyChartRender(t, admin, userCredentialArgs)
	for _, name := range []string{"credentials-records", "credentials-status", "credential-secrets"} {
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
			if !seen[kind+"/patchy-"+name] {
				t.Errorf("the user-credentials render has no %s patchy-%s", kind, name)
			}
		}
	}
	grantCredentialTesters(t, admin)

	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}

	t.Run("rbac", func(t *testing.T) { checkCredentialRBAC(t, admin) })
	t.Run("records", func(t *testing.T) { checkCredentialRecords(t, admin, as) })
	t.Run("status", func(t *testing.T) { checkCredentialStatus(t, admin, as) })
	t.Run("secrets", func(t *testing.T) { checkCredentialSecrets(t, admin, as) })
	t.Run("account store", func(t *testing.T) { checkAccountStore(t, admin, as) })
}

// checkAccountStore drives the status server's Account page store
// (internal/web/credstore) as the status server's own ServiceAccount,
// against the chart's RBAC and policies: it can store, replace and delete a
// person's credential without ever reading a Secret, every write it makes
// passes the policies, and the record's status ends as the design says
// (the new fingerprint, health reset to Unknown for a new credential).
func checkAccountStore(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	status := as(saStatus)
	store := &credstore.Store{Client: status, Reader: status, Namespace: egressRelease,
		CredentialsNamespace: credNamespace}
	owner := v1alpha1.ModelCredentialOwner(31)
	provider := v1alpha1.ModelCredentialProviderAnthropic
	put := func(cred string, kind v1alpha1.ModelCredentialKind) *v1alpha1.ModelCredential {
		t.Helper()
		return putAs(t, store, owner, provider, cred, kind)
	}
	stored := func() string {
		t.Helper()
		return storedCredential(t, admin, owner, provider)
	}

	first := put("sk-ant-api03-first-credential-aaaa", v1alpha1.ModelCredentialAPIKey)
	if got := stored(); got != "sk-ant-api03-first-credential-aaaa" {
		t.Fatalf("stored %q", got)
	}
	checkRecord(t, first, owner, v1alpha1.ModelCredentialAPIKey, "sk-ant-api03-first-credential-aaaa")

	// The broker found it Valid; replacing it (an unconditional update of
	// a Secret the status server cannot read) resets the health for the
	// new credential and changes the kind.
	var rec v1alpha1.ModelCredential
	if err := admin.Get(ctx, client.ObjectKeyFromObject(first), &rec); err != nil {
		t.Fatal(err)
	}
	rec.Status.Health = v1alpha1.ModelCredentialValid
	rec.Status.SecretVersion = "1"
	if err := as(saBroker).Status().Update(ctx, &rec); err != nil {
		t.Fatalf("the broker's verdict: %v", err)
	}
	second := put("sk-ant-oat01-second-credential-bbbb", v1alpha1.ModelCredentialOAuthToken)
	if got := stored(); got != "sk-ant-oat01-second-credential-bbbb" {
		t.Fatalf("replaced with %q", got)
	}
	checkRecord(t, second, owner, v1alpha1.ModelCredentialOAuthToken, "sk-ant-oat01-second-credential-bbbb")

	// The same person's OpenAI key is a record and Secret of its own, beside
	// the Anthropic one, and outlives its deletion.
	openai := v1alpha1.ModelCredentialProviderOpenAI
	third := putAs(t, store, owner, openai, "sk-proj-third-credential-cccc", v1alpha1.ModelCredentialAPIKey)
	if got := storedCredential(t, admin, owner, openai); got != "sk-proj-third-credential-cccc" {
		t.Fatalf("stored openai %q", got)
	}
	checkRecord(t, third, owner, v1alpha1.ModelCredentialAPIKey, "sk-proj-third-credential-cccc")

	checkAccountDelete(t, admin, store, owner, provider)
	if got := storedCredential(t, admin, owner, openai); got != "sk-proj-third-credential-cccc" {
		t.Fatalf("the openai credential after deleting the anthropic one = %q", got)
	}
	checkAccountDelete(t, admin, store, owner, openai)
}

// putAs stores cred for owner and provider through the Account page store,
// waiting out the RBAC informer window.
func putAs(t *testing.T, store *credstore.Store, owner string, provider v1alpha1.ModelCredentialProvider,
	cred string, kind v1alpha1.ModelCredentialKind) *v1alpha1.ModelCredential {
	t.Helper()
	ctx := t.Context()
	var rec *v1alpha1.ModelCredential
	waitFor(t, func() error {
		var err error
		rec, err = store.Put(ctx, web.CredentialWrite{Owner: owner, OwnerLogin: "erin", Provider: provider,
			Kind: kind, Credential: cred, By: "github:erin"})
		if err != nil {
			return &waitError{err: err, want: "no error"}
		}
		return nil
	})
	return rec
}

// storedCredential reads owner's stored credential for provider as an
// admin, checking its owner label.
func storedCredential(t *testing.T, admin client.Client, owner string, provider v1alpha1.ModelCredentialProvider) string {
	t.Helper()
	var s corev1.Secret
	if err := admin.Get(t.Context(), types.NamespacedName{Namespace: credNamespace,
		Name: v1alpha1.ModelCredentialSecretName(owner, provider)}, &s); err != nil {
		t.Fatalf("read the stored secret: %v", err)
	}
	if s.Labels[v1alpha1.LabelCredentialOwner] != v1alpha1.ModelCredentialOwnerHash(owner) {
		t.Errorf("owner label = %v", s.Labels)
	}
	return string(s.Data[v1alpha1.ModelCredentialSecretKey])
}

// checkRecord: a record as the Account page leaves it after storing cred
// as kind: the owner's, its fingerprint, and health Unknown for the
// Secret version no probe has judged.
func checkRecord(t *testing.T, rec *v1alpha1.ModelCredential, owner string, kind v1alpha1.ModelCredentialKind,
	cred string) {
	t.Helper()
	if rec.Spec.Owner != owner || rec.Spec.Kind != kind ||
		rec.Status.Fingerprint != v1alpha1.ModelCredentialFingerprint([]byte(cred)) ||
		rec.Status.Health != v1alpha1.ModelCredentialUnknown || rec.Status.SecretVersion != "" {
		t.Fatalf("record after storing %s = %+v", kind, rec)
	}
}

// checkAccountDelete: deleting removes the Secret and the record, and
// deleting again is no error.
func checkAccountDelete(t *testing.T, admin client.Client, store *credstore.Store, owner string,
	provider v1alpha1.ModelCredentialProvider) {
	t.Helper()
	ctx := t.Context()
	for range 2 {
		if err := store.Delete(ctx, owner, provider); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if got, err := store.Record(ctx, owner, provider); err != nil || got != nil {
		t.Fatalf("record after delete = %+v, %v", got, err)
	}
	var gone corev1.Secret
	if err := admin.Get(ctx, types.NamespacedName{Namespace: credNamespace,
		Name: v1alpha1.ModelCredentialSecretName(owner, provider)}, &gone); !apierrors.IsNotFound(err) {
		t.Fatalf("secret after delete: %v", err)
	}
}

// checkCredentialRBAC: what the chart grants, and what it must not.
func checkCredentialRBAC(t *testing.T, admin client.Client) {
	group := v1alpha1.GroupVersion.Group
	secretsIn := func(ns string) authorizationv1.ResourceAttributes {
		return authorizationv1.ResourceAttributes{Namespace: ns, Resource: "secrets"}
	}
	records := authorizationv1.ResourceAttributes{Namespace: egressRelease, Group: group, Resource: "modelcredentials"}
	status := records
	status.Subresource = "status"
	allowed := rbactest.Verbs(saStatus, nil, secretsIn(credNamespace), "create", "update", "delete")
	allowed = append(allowed, rbactest.Verbs(saStatus, nil, records, "get", "list", "create", "update", "delete")...)
	allowed = append(allowed, rbactest.Verbs(saStatus, nil, status, "update", "patch")...)
	allowed = append(allowed, rbactest.Verbs(saBroker, nil, secretsIn(credNamespace), "get", "list", "watch")...)
	allowed = append(allowed, rbactest.Verbs(saBroker, nil, records, "get", "list", "watch")...)
	allowed = append(allowed, rbactest.Verbs(saBroker, nil, status, "patch")...)
	allowed = append(allowed, rbactest.Review(saBroker, nil, authorizationv1.ResourceAttributes{
		Namespace: egressAgents, Resource: "pods", Verb: "get",
	}))
	rbactest.WaitAllowed(t, admin, allowed...)

	// No patch either: a PATCH response is the whole stored object, so
	// even a no-op patch would read a key back.
	denied := rbactest.Verbs(saStatus, nil, secretsIn(credNamespace), "get", "list", "watch", "patch")
	denied = append(denied, rbactest.Verbs(saStatus, nil, secretsIn(egressRelease), "create", "get")...)
	denied = append(denied, rbactest.Verbs(saBroker, nil, secretsIn(egressRelease), "get")...)
	denied = append(denied, rbactest.Verbs(saBroker, nil, secretsIn(credNamespace), "create", "update")...)
	denied = append(denied, rbactest.Verbs(saBroker, nil, records, "create", "update", "delete")...)
	denied = append(denied, rbactest.Verbs(saBroker, nil, authorizationv1.ResourceAttributes{
		Namespace: egressAgents, Resource: "pods",
	}, "list", "watch")...)
	for _, spec := range denied {
		if sarAllowed(t, admin, spec) {
			a := spec.ResourceAttributes
			t.Errorf("%s may %s %s in %s; the chart must not grant it", spec.User, a.Verb, a.Resource, a.Namespace)
		}
	}
}

func checkCredentialRecords(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	user := as(credUser)
	waitFor(t, func() error {
		err := user.Create(ctx, credentialRecord(7), client.DryRunAll)
		if deniedWith(err, credRecordDenied) {
			return nil
		}
		return &waitError{err: err, want: credRecordDenied}
	})
	for _, tc := range []struct {
		name string
		c    client.Client
		want string
	}{
		{"the status server", as(saStatus), ""},
		{"a credentials admin", as(credAdmin), ""},
		{"cluster-admin", admin, ""},
		{"a user with a broad grant", user, credRecordDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Create(ctx, credentialRecord(8), client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}

	rec := credentialRecord(9)
	if err := as(saStatus).Create(ctx, rec); err != nil {
		t.Fatalf("create as the status server: %v", err)
	}
	rec.Spec.Kind = v1alpha1.ModelCredentialOAuthToken
	if err := user.Update(ctx, rec, client.DryRunAll); !deniedWith(err, credRecordDenied) {
		t.Errorf("a user rewriting someone's record: got %v, want refused", err)
	}
	if err := as(saStatus).Update(ctx, rec, client.DryRunAll); err != nil {
		t.Errorf("the status server replacing a record's kind was refused: %v", err)
	}
	// Deleting is a revocation, left to RBAC.
	if err := user.Delete(ctx, rec, client.DryRunAll); err != nil {
		t.Errorf("a user holding delete was refused a revocation: %v", err)
	}
}

func checkCredentialStatus(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	rec := credentialRecord(10)
	if err := as(saStatus).Create(ctx, rec); err != nil {
		t.Fatalf("create as the status server: %v", err)
	}
	key := client.ObjectKeyFromObject(rec)
	// write applies mutate to the record's current status as c.
	write := func(c client.Client, mutate func(*v1alpha1.ModelCredentialStatus)) error {
		cur := &v1alpha1.ModelCredential{}
		if err := admin.Get(ctx, key, cur); err != nil {
			t.Fatalf("get record: %v", err)
		}
		mutate(&cur.Status)
		return c.Status().Update(ctx, cur)
	}
	fingerprint := v1alpha1.ModelCredentialFingerprint([]byte("sk-ant-api03-abcdefghijklmnop"))
	other := v1alpha1.ModelCredentialFingerprint([]byte("sk-ant-api03-qrstuvwxyz012345"))
	now := metav1.Now()

	waitFor(t, func() error {
		err := write(as(credUser), func(s *v1alpha1.ModelCredentialStatus) { s.Health = v1alpha1.ModelCredentialValid })
		if deniedWith(err, credStatusWriterDenied) {
			return nil
		}
		return &waitError{err: err, want: credStatusWriterDenied}
	})
	steps := []struct {
		name   string
		c      client.Client
		mutate func(*v1alpha1.ModelCredentialStatus)
		want   string
	}{
		{"the status server stores a fingerprint, health Unknown", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) {
				s.Fingerprint, s.Health = fingerprint, v1alpha1.ModelCredentialUnknown
			}, ""},
		{"the status server claiming Valid", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) { s.Health = v1alpha1.ModelCredentialValid }, credStatusResetDenied},
		{"the broker's probe finds it Valid", as(saBroker),
			func(s *v1alpha1.ModelCredentialStatus) {
				s.Health, s.LastUsedAt = v1alpha1.ModelCredentialValid, &now
			}, ""},
		{"the broker changing the fingerprint", as(saBroker),
			func(s *v1alpha1.ModelCredentialStatus) { s.Fingerprint = other }, credFingerprintDenied},
		{"the status server moving lastUsedAt", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) { s.LastUsedAt = nil }, credStatusResetDenied},
		{"the status server stores a new key, health reset", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) {
				s.Fingerprint, s.Health = other, v1alpha1.ModelCredentialUnknown
			}, ""},
		{"the broker's probe finds it Rejected", as(saBroker),
			func(s *v1alpha1.ModelCredentialStatus) {
				s.Health, s.LastRejectedAt = v1alpha1.ModelCredentialRejected, &now
			}, ""},
		{"the broker judges it against the Secret version it probed", as(saBroker),
			func(s *v1alpha1.ModelCredentialStatus) { s.SecretVersion = "41" }, ""},
		{"the status server claiming another Secret version", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) { s.SecretVersion = "42" }, credStatusResetDenied},
		{"the status server clearing the Secret version", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) {
				s.SecretVersion, s.Health = "", v1alpha1.ModelCredentialUnknown
			}, ""},
		{"the status server clearing a rejection", as(saStatus),
			func(s *v1alpha1.ModelCredentialStatus) { s.LastRejectedAt = nil }, credStatusResetDenied},
		{"a credentials admin", as(credAdmin),
			func(s *v1alpha1.ModelCredentialStatus) { s.Health = v1alpha1.ModelCredentialValid }, credStatusWriterDenied},
		{"cluster-admin", admin,
			func(s *v1alpha1.ModelCredentialStatus) { s.Health = v1alpha1.ModelCredentialValid }, credStatusWriterDenied},
	}
	for _, step := range steps {
		err := write(step.c, step.mutate)
		switch {
		case step.want == "" && err != nil:
			t.Errorf("%s: refused, want admitted: %v", step.name, err)
		case step.want != "" && !deniedWith(err, step.want):
			t.Errorf("%s: got %v, want refused with %q", step.name, err, step.want)
		}
	}
}

func checkCredentialSecrets(t *testing.T, admin client.Client, as func(string) client.Client) {
	ctx := t.Context()
	status := as(saStatus)
	waitFor(t, func() error {
		err := as(credUser).Create(ctx, credentialSecret(20, nil), client.DryRunAll)
		if deniedWith(err, credSecretWriterDenied) {
			return nil
		}
		return &waitError{err: err, want: credSecretWriterDenied}
	})
	owner := v1alpha1.ModelCredentialOwner(21)
	otherHash := v1alpha1.ModelCredentialOwnerHash(v1alpha1.ModelCredentialOwner(22))
	for _, tc := range []struct {
		name   string
		c      client.Client
		mutate func(*corev1.Secret)
		want   string
	}{
		{"the status server, well formed", status, nil, ""},
		{"a credentials admin, well formed", as(credAdmin), nil, ""},
		{"cluster-admin, well formed", admin, nil, ""},
		{"a user with a broad grant", as(credUser), nil, credSecretWriterDenied},
		{"the broker", as(saBroker), nil, "forbidden"},
		{"no owner label", status, func(s *corev1.Secret) { s.Labels = nil }, credSecretNameDenied},
		{"another person's owner label", status, func(s *corev1.Secret) {
			s.Labels[v1alpha1.LabelCredentialOwner] = otherHash
		}, credSecretNameDenied},
		{"a name of the caller's choosing", status, func(s *corev1.Secret) { s.Name = "anthropic-key" },
			credSecretNameDenied},
		{"an openai credential", status, func(s *corev1.Secret) {
			s.Name = v1alpha1.ModelCredentialSecretName(owner, v1alpha1.ModelCredentialProviderOpenAI)
			s.Data[v1alpha1.ModelCredentialSecretKey] = []byte("sk-proj-abcdefghijklmnop")
		}, ""},
		{"a provider outside the enum", status, func(s *corev1.Secret) {
			s.Name = "cred-" + v1alpha1.ModelCredentialOwnerHash(owner) + "-bedrock"
		}, credSecretNameDenied},
		{"a second key", status, func(s *corev1.Secret) { s.Data["extra"] = []byte("x") }, credSecretDataDenied},
		{"another key name", status, func(s *corev1.Secret) {
			s.Data = map[string][]byte{"api-key": []byte("sk-ant-api03-abcdefghijklmnop")}
		}, credSecretDataDenied},
		{"an empty credential", status, func(s *corev1.Secret) {
			s.Data[v1alpha1.ModelCredentialSecretKey] = []byte{}
		}, credSecretDataDenied},
		{"the largest credential", status, func(s *corev1.Secret) {
			s.Data[v1alpha1.ModelCredentialSecretKey] = []byte(strings.Repeat("k", v1alpha1.ModelCredentialSecretMaxBytes))
		}, ""},
		{"one byte too large", status, func(s *corev1.Secret) {
			s.Data[v1alpha1.ModelCredentialSecretKey] = []byte(strings.Repeat("k",
				v1alpha1.ModelCredentialSecretMaxBytes+1))
		}, credSecretDataDenied},
		{"stringData", status, func(s *corev1.Secret) {
			s.Data = nil
			s.StringData = map[string]string{v1alpha1.ModelCredentialSecretKey: "sk-ant-api03-abcdefghijklmnop"}
		}, ""},
		{"another type", status, func(s *corev1.Secret) { s.Type = "example.com/credential" }, credSecretDataDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.c.Create(ctx, credentialSecret(21, tc.mutate), client.DryRunAll)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused, want admitted: %v", err)
			case tc.want != "" && !deniedWith(err, tc.want):
				t.Errorf("got %v, want refused with %q", err, tc.want)
			}
		})
	}

	// UPDATE: the status server replaces its own Secret; nobody may move
	// the label off its name, and a broad grant may not touch it.
	s := credentialSecret(23, nil)
	if err := status.Create(ctx, s); err != nil {
		t.Fatalf("create as the status server: %v", err)
	}
	s.Data[v1alpha1.ModelCredentialSecretKey] = []byte("sk-ant-oat01-replacement-token")
	if err := status.Update(ctx, s); err != nil {
		t.Errorf("the status server replacing a credential was refused: %v", err)
	}
	// The status server may not read a stored key back through a PATCH,
	// whose response is the whole object: a no-op dry-run patch is
	// refused by RBAC before admission (which would admit the unchanged,
	// well-formed Secret).
	noop := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: s.Namespace}}
	err := status.Patch(ctx, noop, client.RawPatch(types.MergePatchType, []byte("{}")), client.DryRunAll)
	if !apierrors.IsForbidden(err) {
		t.Errorf("a no-op patch of a credential Secret as the status server: got %v, want Forbidden", err)
	}
	if len(noop.Data) != 0 || len(noop.StringData) != 0 {
		t.Error("a refused patch handed the status server a stored credential")
	}
	relabel := s.DeepCopy()
	relabel.Labels[v1alpha1.LabelCredentialOwner] = otherHash
	if err := admin.Update(ctx, relabel, client.DryRunAll); !deniedWith(err, credSecretNameDenied) {
		t.Errorf("relabelling a credential for another person: got %v, want refused", err)
	}
	if err := as(credUser).Update(ctx, s, client.DryRunAll); !deniedWith(err, credSecretWriterDenied) {
		t.Errorf("a broad grant replacing someone's credential: got %v, want refused", err)
	}

	// The policy is bound to the credentials namespace only.
	elsewhere := credentialSecret(24, func(s *corev1.Secret) {
		s.Namespace, s.Name, s.Labels = egressRelease, "unrelated", nil
	})
	if err := admin.Create(ctx, elsewhere, client.DryRunAll); err != nil {
		t.Errorf("a Secret outside the credentials namespace was judged: %v", err)
	}
}

// grantCredentialTesters gives credUser and credAdmin everything RBAC can
// give on the records and their Secrets, and binds credAdmin to the chart's
// -credentials-admin ClusterRole, so each case is decided by admission.
func grantCredentialTesters(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	subjects := []rbacv1.Subject{
		{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: credUser},
		{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: credAdmin},
	}
	all := []string{"get", "list", "create", "update", "patch", "delete"}
	objs := []client.Object{
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-tester", Namespace: egressRelease},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{v1alpha1.GroupVersion.Group},
				Resources: []string{"modelcredentials", "modelcredentials/status"},
				Verbs:     all,
			}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-tester", Namespace: egressRelease},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "credential-tester"},
			Subjects:   subjects,
		},
		&rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-tester", Namespace: credNamespace},
			Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: all}},
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-tester", Namespace: credNamespace},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: "credential-tester"},
			Subjects:   subjects,
		},
		&rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "credential-admin", Namespace: egressRelease},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole",
				Name: "patchy-credentials-admin"},
			Subjects: subjects[1:],
		},
	}
	for _, obj := range objs {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatalf("create %T: %v", obj, err)
		}
	}
	group := v1alpha1.GroupVersion.Group
	reviews := make([]authorizationv1.SubjectAccessReviewSpec, 0, 13)
	for _, u := range []string{credUser, credAdmin} {
		reviews = append(reviews, rbactest.Verbs(u, nil, authorizationv1.ResourceAttributes{
			Namespace: egressRelease, Group: group, Resource: "modelcredentials",
		}, "create", "update", "delete")...)
		reviews = append(reviews, rbactest.Verbs(u, nil, authorizationv1.ResourceAttributes{
			Namespace: egressRelease, Group: group, Resource: "modelcredentials", Subresource: "status",
		}, "update")...)
		reviews = append(reviews, rbactest.Verbs(u, nil, authorizationv1.ResourceAttributes{
			Namespace: credNamespace, Resource: "secrets",
		}, "create", "update")...)
	}
	reviews = append(reviews, rbactest.Review(credAdmin, nil, authorizationv1.ResourceAttributes{
		Namespace: egressRelease, Group: group, Resource: "modelcredentials", Subresource: "all", Verb: "update",
	}))
	rbactest.WaitAllowed(t, admin, reviews...)
	if sarAllowed(t, admin, rbactest.Review(credUser, nil, authorizationv1.ResourceAttributes{
		Namespace: egressRelease, Group: group, Resource: "modelcredentials", Subresource: "all", Verb: "update",
	})) {
		t.Fatal("the broad grant reaches modelcredentials/all; the user cases would test nothing")
	}
}

// credentialRecord is GitHub id id's anthropic ModelCredential.
func credentialRecord(id int64) *v1alpha1.ModelCredential {
	owner := v1alpha1.ModelCredentialOwner(id)
	return &v1alpha1.ModelCredential{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.ModelCredentialName(owner, v1alpha1.ModelCredentialProviderAnthropic),
			Namespace: egressRelease,
		},
		Spec: v1alpha1.ModelCredentialSpec{
			Owner: owner, OwnerLogin: "octocat",
			Provider: v1alpha1.ModelCredentialProviderAnthropic, Kind: v1alpha1.ModelCredentialAPIKey,
		},
	}
}

// credentialSecret is GitHub id id's anthropic credential Secret, as the
// status server writes it, after mutate (when non-nil).
func credentialSecret(id int64, mutate func(*corev1.Secret)) *corev1.Secret {
	owner := v1alpha1.ModelCredentialOwner(id)
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      v1alpha1.ModelCredentialSecretName(owner, v1alpha1.ModelCredentialProviderAnthropic),
			Namespace: credNamespace,
			Labels:    map[string]string{v1alpha1.LabelCredentialOwner: v1alpha1.ModelCredentialOwnerHash(owner)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{v1alpha1.ModelCredentialSecretKey: []byte("sk-ant-api03-abcdefghijklmnop")},
	}
	if mutate != nil {
		mutate(s)
	}
	return s
}

// sarAllowed asks the API server's authorizer once.
func sarAllowed(t *testing.T, admin client.Client, spec authorizationv1.SubjectAccessReviewSpec) bool {
	t.Helper()
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&authorizationv1.SubjectAccessReview{Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	sar := &unstructured.Unstructured{Object: raw}
	sar.SetGroupVersionKind(authorizationv1.SchemeGroupVersion.WithKind("SubjectAccessReview"))
	if err := admin.Create(t.Context(), sar); err != nil {
		t.Fatalf("SubjectAccessReview: %v", err)
	}
	allowed, _, _ := unstructured.NestedBool(sar.Object, "status", "allowed")
	return allowed
}
