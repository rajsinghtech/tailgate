// Package ui implements the tailgate-ui HTTP server: a small HTMX-driven web
// UI for managing EgressGroups. Users authenticate via Tailscale OAuth (handled
// by internal/auth), and the UI reads/writes EgressGroup CRs through the kube
// API. Authorization is owner-based: each EgressGroup carries an
// `tailgate.dev/owner` annotation set at create time; non-admins see only their
// own groups.
package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	egressv1 "github.com/rajsinghtech/tailgate/api/v1alpha1"
	"github.com/rajsinghtech/tailgate/internal/auth"
)

const ownerAnnotation = "tailgate.dev/owner"

// Server is the UI HTTP server.
type Server struct {
	kc     client.Client
	scheme *runtime.Scheme
	auth   *auth.Handler
	log    *slog.Logger
	tmpl   *template.Template
}

// NewServer returns a UI server wired to the given kube client and auth handler.
func NewServer(kc client.Client, scheme *runtime.Scheme, authHandler *auth.Handler, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		kc:     kc,
		scheme: scheme,
		auth:   authHandler,
		log:    log,
	}
	s.tmpl = template.Must(template.New("").Funcs(funcMap).Parse(tmplStr))
	return s
}

// Handler returns the root http.Handler with all routes mounted.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Auth routes (unauthenticated).
	mux.Handle("/login", s.auth)
	mux.Handle("/callback", s.auth)
	mux.Handle("/logout", s.auth)

	// Static.
	mux.HandleFunc("/static/style.css", s.serveCSS)

	// Authenticated routes.
	authed := s.auth.Middleware(http.HandlerFunc(s.handleAuthed))
	mux.Handle("/", authed)
	mux.Handle("/groups/", authed)
	mux.Handle("/groups/new", authed)
	mux.Handle("/groups/create", authed)
	mux.Handle("/groups/delete/", authed)

	return mux
}

func (s *Server) serveCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(cssStr))
}

func (s *Server) handleAuthed(w http.ResponseWriter, r *http.Request) {
	sess := auth.SessionFromContext(r.Context())
	if sess == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		s.listGroups(w, r, sess)
	case r.URL.Path == "/groups/new" && r.Method == http.MethodGet:
		s.newGroupForm(w, r, sess)
	case r.URL.Path == "/groups/create" && r.Method == http.MethodPost:
		s.createGroup(w, r, sess)
	case strings.HasPrefix(r.URL.Path, "/groups/delete/") && r.Method == http.MethodPost:
		s.deleteGroup(w, r, sess)
	case strings.HasPrefix(r.URL.Path, "/groups/") && r.Method == http.MethodGet:
		s.viewGroup(w, r, sess)
	default:
		http.NotFound(w, r)
	}
}

type groupView struct {
	Name         string
	Owner        string
	Pods         int32
	Nodes        int32
	Gateway      string
	ExitNode     string
	DNS          bool
	AcceptRoutes bool
	Tags         []string
	GatewayNodes []string
	Created      time.Time
	Age          string
}

type listData struct {
	Session    *auth.Session
	IsAdmin    bool
	Groups     []groupView
	TotalNodes int
}

func (s *Server) listGroups(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	ctx := r.Context()
	var groups egressv1.EgressGroupList
	if err := s.kc.List(ctx, &groups); err != nil {
		s.log.Error("list groups", "err", err)
		s.renderError(w, "Failed to list groups", http.StatusInternalServerError)
		return
	}

	isAdmin := s.auth.IsAdmin(sess)
	var views []groupView
	for i := range groups.Items {
		g := &groups.Items[i]
		owner := g.Annotations[ownerAnnotation]
		if !isAdmin && !strings.EqualFold(owner, sess.Email) {
			continue
		}
		views = append(views, groupView{
			Name:         g.Name,
			Owner:        owner,
			Pods:         g.Status.MatchedPods,
			Nodes:        int32(len(g.Status.GatewayNodes)),
			Gateway:      g.Status.GatewayHostname,
			ExitNode:     g.Status.ResolvedExitNode,
			DNS:          g.Spec.DNS != nil && g.Spec.DNS.Enabled,
			AcceptRoutes: g.Spec.AcceptRoutesEnabled(),
			Tags:         g.Spec.Tags,
			GatewayNodes: g.Status.GatewayNodes,
			Created:      g.CreationTimestamp.Time,
			Age:          ageStr(g.CreationTimestamp.Time),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })

	s.render(w, "list", listData{Session: sess, IsAdmin: isAdmin, Groups: views, TotalNodes: len(views)})
}

type formData struct {
	Session *auth.Session
	IsAdmin bool
}

func (s *Server) newGroupForm(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	s.render(w, "form", formData{Session: sess, IsAdmin: s.auth.IsAdmin(sess)})
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	if err := r.ParseForm(); err != nil {
		s.renderError(w, "Invalid form data", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.renderError(w, "Name is required", http.StatusBadRequest)
		return
	}

	eg := &egressv1.EgressGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				ownerAnnotation: sess.Email,
			},
		},
		Spec: egressv1.EgressGroupSpec{
			Selector: egressv1.EgressSelector{
				PodSelector: parseLabelSelector(r.FormValue("podLabels")),
			},
		},
	}

	if tags := strings.TrimSpace(r.FormValue("tags")); tags != "" {
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				eg.Spec.Tags = append(eg.Spec.Tags, t)
			}
		}
	}

	if r.FormValue("acceptRoutes") == "" {
		b := false
		eg.Spec.AcceptRoutes = &b
	}

	if r.FormValue("dnsEnabled") == "on" {
		eg.Spec.DNS = &egressv1.MemberDNS{Enabled: true}
	}

	if exit := strings.TrimSpace(r.FormValue("exitNode")); exit != "" {
		eg.Spec.ExitNode = &egressv1.ExitNodeRef{Name: exit}
	}

	if ns := strings.TrimSpace(r.FormValue("nodeSelector")); ns != "" {
		eg.Spec.Gateway = &egressv1.GatewaySpec{NodeSelector: parseLabelMap(ns)}
	}

	ctx := r.Context()
	if err := s.kc.Create(ctx, eg); err != nil {
		s.log.Error("create group", "err", err, "name", name)
		s.renderError(w, "Failed to create group: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("created egressgroup", "name", name, "owner", sess.Email)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	name := strings.TrimPrefix(r.URL.Path, "/groups/delete/")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	var eg egressv1.EgressGroup
	if err := s.kc.Get(ctx, types.NamespacedName{Name: name}, &eg); err != nil {
		s.renderError(w, "Group not found", http.StatusNotFound)
		return
	}

	owner := eg.Annotations[ownerAnnotation]
	if !s.auth.IsAdmin(sess) && !strings.EqualFold(owner, sess.Email) {
		s.renderError(w, "Not authorized to delete this group", http.StatusForbidden)
		return
	}

	if err := s.kc.Delete(ctx, &eg); err != nil {
		s.log.Error("delete group", "err", err, "name", name)
		s.renderError(w, "Failed to delete: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("deleted egressgroup", "name", name, "by", sess.Email)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) viewGroup(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	name := strings.TrimPrefix(r.URL.Path, "/groups/")
	if name == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	var eg egressv1.EgressGroup
	if err := s.kc.Get(ctx, types.NamespacedName{Name: name}, &eg); err != nil {
		s.renderError(w, "Group not found", http.StatusNotFound)
		return
	}

	owner := eg.Annotations[ownerAnnotation]
	if !s.auth.IsAdmin(sess) && !strings.EqualFold(owner, sess.Email) {
		s.renderError(w, "Not authorized to view this group", http.StatusForbidden)
		return
	}

	// Count member pods by listing pods matching the selector.
	memberPods := countMemberPods(ctx, s.kc, &eg)

	v := groupView{
		Name:         eg.Name,
		Owner:        owner,
		Pods:         memberPods,
		Nodes:        int32(len(eg.Status.GatewayNodes)),
		Gateway:      eg.Status.GatewayHostname,
		ExitNode:     eg.Status.ResolvedExitNode,
		DNS:          eg.Spec.DNS != nil && eg.Spec.DNS.Enabled,
		AcceptRoutes: eg.Spec.AcceptRoutesEnabled(),
		Tags:         eg.Spec.Tags,
		GatewayNodes: eg.Status.GatewayNodes,
		Created:      eg.CreationTimestamp.Time,
		Age:          ageStr(eg.CreationTimestamp.Time),
	}

	s.render(w, "detail", struct {
		Session *auth.Session
		IsAdmin bool
		Group   groupView
	}{Session: sess, IsAdmin: s.auth.IsAdmin(sess), Group: v})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template", "err", err, "name", name)
	}
}

func (s *Server) renderError(w http.ResponseWriter, msg string, code int) {
	w.WriteHeader(code)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "error", map[string]any{"Message": msg}); err != nil {
		_, _ = w.Write([]byte(msg))
	}
}

func countMemberPods(ctx context.Context, kc client.Client, eg *egressv1.EgressGroup) int32 {
	sel := eg.Spec.Selector
	if sel.PodSelector == nil && sel.NamespaceSelector == nil {
		return 0
	}
	var pods corev1.PodList
	if err := kc.List(ctx, &pods); err != nil {
		return 0
	}
	var count int32
	if sel.PodSelector != nil {
		ls, err := metav1.LabelSelectorAsSelector(sel.PodSelector)
		if err != nil {
			return 0
		}
		for i := range pods.Items {
			if ls.Matches(labels.Set(pods.Items[i].Labels)) {
				count++
			}
		}
	}
	return count
}

func parseLabelSelector(s string) *metav1.LabelSelector {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	ls := &metav1.LabelSelector{MatchLabels: map[string]string{}}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		ls.MatchLabels[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(ls.MatchLabels) == 0 {
		return nil
	}
	return ls
}

func parseLabelMap(s string) map[string]string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	m := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

func ageStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

var funcMap = template.FuncMap{
	"json": func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	},
	"join": strings.Join,
}
