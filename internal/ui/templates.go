package ui

const cssStr = `
:root { --bg: #0d1117; --surface: #161b22; --border: #30363d; --text: #e6edf3; --muted: #7d8590; --accent: #2f81f7; --green: #3fb950; --red: #f85149; --yellow: #d29922; }
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, system-ui, sans-serif; background: var(--bg); color: var(--text); line-height: 1.5; }
a { color: var(--accent); text-decoration: none; }
a:hover { text-decoration: underline; }
.nav { display: flex; align-items: center; justify-content: space-between; padding: 12px 24px; border-bottom: 1px solid var(--border); background: var(--surface); }
.nav-brand { font-weight: 700; font-size: 1.1rem; }
.nav-brand a { color: var(--text); }
.nav-user { display: flex; align-items: center; gap: 12px; color: var(--muted); font-size: .9rem; }
.nav-user .badge { background: var(--border); padding: 2px 8px; border-radius: 4px; font-size: .8rem; }
.container { max-width: 960px; margin: 0 auto; padding: 24px; }
.card { background: var(--surface); border: 1px solid var(--border); border-radius: 8px; padding: 16px; margin-bottom: 16px; }
.card-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 12px; }
.card-header h2 { font-size: 1.1rem; }
.grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); gap: 16px; }
.group-card { background: var(--surface); border: 1px solid var(--border); border-radius: 8px; padding: 16px; }
.group-card h3 { font-size: 1rem; margin-bottom: 8px; }
.group-card .meta { color: var(--muted); font-size: .85rem; display: flex; flex-wrap: wrap; gap: 8px 16px; }
.group-card .meta span { display: inline-flex; align-items: center; gap: 4px; }
.pill { display: inline-block; padding: 2px 8px; border-radius: 12px; font-size: .75rem; font-weight: 600; }
.pill-green { background: rgba(63,185,80,.15); color: var(--green); }
.pill-yellow { background: rgba(210,153,34,.15); color: var(--yellow); }
.pill-red { background: rgba(248,81,73,.15); color: var(--red); }
.pill-blue { background: rgba(47,129,247,.15); color: var(--accent); }
.btn { display: inline-block; padding: 6px 16px; border-radius: 6px; font-size: .9rem; font-weight: 600; border: 1px solid var(--border); background: var(--surface); color: var(--text); cursor: pointer; }
.btn:hover { border-color: var(--accent); }
.btn-primary { background: var(--accent); border-color: var(--accent); color: #fff; }
.btn-danger { color: var(--red); border-color: var(--red); }
.form-group { margin-bottom: 16px; }
.form-group label { display: block; font-size: .85rem; color: var(--muted); margin-bottom: 4px; }
.form-group input, .form-group select, .form-group textarea { width: 100%; padding: 8px 12px; background: var(--bg); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: .9rem; }
.form-group input:focus { border-color: var(--accent); outline: none; }
.form-group .hint { font-size: .8rem; color: var(--muted); margin-top: 4px; }
.checkbox { display: flex; align-items: center; gap: 8px; }
.checkbox input { width: auto; }
.empty { text-align: center; padding: 48px; color: var(--muted); }
.empty h3 { font-size: 1.1rem; margin-bottom: 8px; }
table { width: 100%; border-collapse: collapse; }
th, td { padding: 8px 12px; text-align: left; border-bottom: 1px solid var(--border); font-size: .9rem; }
th { color: var(--muted); font-weight: 600; font-size: .8rem; text-transform: uppercase; }
.detail-row { display: flex; padding: 8px 0; border-bottom: 1px solid var(--border); }
.detail-label { width: 180px; color: var(--muted); font-size: .85rem; }
.detail-value { flex: 1; }
.error-page { text-align: center; padding: 64px; }
.error-page h1 { font-size: 1.5rem; margin-bottom: 8px; color: var(--red); }
.node-list { display: flex; flex-wrap: wrap; gap: 4px; }
.node-list .pill { font-family: monospace; }
`

const tmplStr = `
{{define "nav"}}
<nav class="nav">
  <div class="nav-brand"><a href="/">tailgate</a></div>
  <div class="nav-user">
    <span>{{.Session.Email}}</span>
    {{if .IsAdmin}}<span class="badge">admin</span>{{end}}
    <a href="/logout">Sign out</a>
  </div>
</nav>
{{end}}

{{define "list"}}
{{template "nav" .}}
<div class="container">
  <div class="card-header">
    <h2>Egress Groups {{if .IsAdmin}}<span class="pill pill-blue">all</span>{{end}}</h2>
    <a href="/groups/new" class="btn btn-primary">+ New Group</a>
  </div>
  {{if .Groups}}
  <div class="grid">
    {{range .Groups}}
    <a href="/groups/{{.Name}}" class="group-card" style="text-decoration:none;color:inherit">
      <h3>{{.Name}}</h3>
      <div class="meta">
        <span>📦 {{.Pods}} pods</span>
        <span>🖥️ {{.Nodes}} nodes</span>
        {{if .DNS}}<span class="pill pill-green">DNS</span>{{end}}
        {{if .ExitNode}}<span class="pill pill-yellow">exit</span>{{end}}
        {{if not .AcceptRoutes}}<span class="pill pill-red">no-routes</span>{{end}}
        <span>{{.Age}}</span>
      </div>
      {{if .Owner}}<div class="meta" style="margin-top:4px"><span>{{.Owner}}</span></div>{{end}}
    </a>
    {{end}}
  </div>
  {{else}}
  <div class="empty">
    <h3>No egress groups yet</h3>
    <p style="margin-bottom:16px">Create one to give your pods native Tailscale egress.</p>
    <a href="/groups/new" class="btn btn-primary">+ New Group</a>
  </div>
  {{end}}
</div>
{{end}}

{{define "form"}}
{{template "nav" .}}
<div class="container">
  <div class="card-header"><h2>New Egress Group</h2></div>
  <div class="card">
    <form action="/groups/create" method="POST">
      <div class="form-group">
        <label for="name">Name</label>
        <input type="text" id="name" name="name" placeholder="payments" required pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?" />
        <div class="hint">DNS-safe name (lowercase, hyphens). This is the CR + gateway name.</div>
      </div>
      <div class="form-group">
        <label for="podLabels">Pod selector (labels)</label>
        <input type="text" id="podLabels" name="podLabels" placeholder="tailgate.dev/egress=true,app=payments" />
        <div class="hint">Comma-separated key=value labels. Pods matching these get egress.</div>
      </div>
      <div class="form-group">
        <label for="tags">Gateway tags</label>
        <input type="text" id="tags" name="tags" placeholder="tag:k8s" />
        <div class="hint">Comma-separated. The OAuth client must own these. Defaults to tag:k8s.</div>
      </div>
      <div class="form-group checkbox">
        <input type="checkbox" id="acceptRoutes" name="acceptRoutes" checked />
        <label for="acceptRoutes">Accept advertised routes (subnet routers + app connectors)</label>
      </div>
      <div class="form-group checkbox">
        <input type="checkbox" id="dnsEnabled" name="dnsEnabled" />
        <label for="dnsEnabled">Native tailnet DNS (MagicDNS + split-DNS for members)</label>
      </div>
      <div class="form-group">
        <label for="exitNode">Exit node (optional)</label>
        <input type="text" id="exitNode" name="exitNode" placeholder="auto" />
        <div class="hint">A tailnet IP, MagicDNS name, or "auto". Empty = no exit node.</div>
      </div>
      <div class="form-group">
        <label for="nodeSelector">Gateway node selector (optional, for gVisor)</label>
        <input type="text" id="nodeSelector" name="nodeSelector" placeholder="nodepool=gvisor" />
        <div class="hint">Pin the gateway to specific nodes. Comma-separated key=value. Empty = auto-follow member pods.</div>
      </div>
      <button type="submit" class="btn btn-primary">Create</button>
      <a href="/" class="btn">Cancel</a>
    </form>
  </div>
</div>
{{end}}

{{define "detail"}}
{{template "nav" .}}
<div class="container">
  <div class="card-header">
    <h2>{{.Group.Name}}</h2>
    <form action="/groups/delete/{{.Group.Name}}" method="POST"
          onsubmit="return confirm('Delete {{.Group.Name}}? This tears down the gateway and removes the tailnet device.')">
      <button type="submit" class="btn btn-danger">Delete</button>
    </form>
  </div>
  <div class="card">
    <div class="detail-row"><div class="detail-label">Owner</div><div class="detail-value">{{.Group.Owner}}</div></div>
    <div class="detail-row"><div class="detail-label">Matched pods</div><div class="detail-value">{{.Group.Pods}}</div></div>
    <div class="detail-row"><div class="detail-label">Gateway nodes</div><div class="detail-value">
      {{if .Group.GatewayNodes}}
      <div class="node-list">{{range .Group.GatewayNodes}}<span class="pill pill-blue">{{.}}</span>{{end}}</div>
      {{else}}<span style="color:var(--muted)">none (no members or auto-follow hasn't placed the gateway yet)</span>{{end}}
    </div></div>
    <div class="detail-row"><div class="detail-label">Gateway hostname</div><div class="detail-value">{{.Group.Gateway}}</div></div>
    <div class="detail-row"><div class="detail-label">Accept routes</div><div class="detail-value">{{if .Group.AcceptRoutes}}yes{{else}}no{{end}}</div></div>
    <div class="detail-row"><div class="detail-label">DNS</div><div class="detail-value">{{if .Group.DNS}}enabled{{else}}disabled{{end}}</div></div>
    {{if .Group.ExitNode}}<div class="detail-row"><div class="detail-label">Exit node</div><div class="detail-value">{{.Group.ExitNode}}</div></div>{{end}}
    {{if .Group.Tags}}<div class="detail-row"><div class="detail-label">Tags</div><div class="detail-value">{{join .Group.Tags ", "}}</div></div>{{end}}
    <div class="detail-row"><div class="detail-label">Created</div><div class="detail-value">{{.Group.Age}} ago</div></div>
  </div>
  <p><a href="/" class="btn">← Back</a></p>
</div>
{{end}}

{{define "error"}}
<div class="error-page">
  <h1>Error</h1>
  <p>{{.Message}}</p>
  <p style="margin-top:16px"><a href="/" class="btn">← Back</a></p>
</div>
{{end}}
`
