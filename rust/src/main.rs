//! Comparison-only stdio MCP broker. No live-install defaults.
use anyhow::{Result, bail};
use axum::{
    Router,
    extract::Request,
    http::StatusCode,
    middleware::{self, Next},
    response::Response,
};
use rmcp::{
    ErrorData as McpError, RoleServer, ServerHandler,
    model::*,
    service::RequestContext,
    transport::streamable_http_server::{
        StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
    },
};
use rusqlite::{Connection, params};
use std::{
    collections::{BTreeMap, HashMap},
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

mod connectors;
mod shaping;
use connectors::{Connector, Registry};
#[derive(Clone)]
struct Route {
    connector: String,
    upstream_name: String,
    tool: Tool,
}
#[derive(Clone)]
struct Front {
    upstreams: Registry,
    connector: Option<String>,
    options: BTreeMap<String, String>,
    db: Arc<Mutex<Connection>>,
}

fn internal(e: impl std::fmt::Display) -> McpError {
    McpError::internal_error(e.to_string(), None)
}
impl Front {
    // A separate autocommit for each phase, WAL/FULL just like Go's default.
    async fn receipt(
        &self,
        id: String,
        route: Route,
        phase: &'static str,
        state: &'static str,
        elapsed: i64,
    ) -> Result<(), McpError> {
        let db = self.db.clone();
        tokio::task::spawn_blocking(move || -> Result<()> {
            db.lock().map_err(|_| anyhow::anyhow!("receipt mutex poisoned"))?.execute(
                "INSERT INTO receipts(call_id,phase,connector,tool,state,duration_us) VALUES(?,?,?,?,?,?)",
                params![id, phase, route.connector, route.upstream_name, state, elapsed])?;
            Ok(())
        }).await.map_err(internal)?.map_err(internal)
    }
}
impl Front {
    fn selected(&self) -> Vec<(String, Arc<Connector>)> {
        let excluded: Vec<&str> = self
            .options
            .get("exclude")
            .map(|s| s.split(',').collect())
            .unwrap_or_default();
        self.upstreams
            .read()
            .unwrap()
            .iter()
            .filter(|(_, c)| c.spec.clients.is_empty())
            .filter(|(n, _)| {
                self.connector
                    .as_ref()
                    .map(|c| c == *n)
                    .unwrap_or(!excluded.contains(&n.as_str()))
            })
            .map(|(n, c)| (n.clone(), c.clone()))
            .collect()
    }
    fn hidden(&self, c: &Connector) -> bool {
        self.connector.is_none()
            && (c.spec.expose == "broker"
                || self.options.get("ondemand").is_some_and(|v| v == "all"))
    }
    fn discovery_tools(&self) -> Vec<Tool> {
        if !self.selected().iter().any(|(_, c)| self.hidden(c)) {
            return Vec::new();
        }
        [("search_tools","Search on-demand tools",serde_json::json!({"query":{"type":"string"}})),("describe_tool","Describe an on-demand tool",serde_json::json!({"connector":{"type":"string"},"tool":{"type":"string"}})),("call_tool","Call an on-demand tool",serde_json::json!({"connector":{"type":"string"},"tool":{"type":"string"},"arguments":{"type":"object"}}))].into_iter().map(|(name,description,properties)|serde_json::from_value(serde_json::json!({"name":name,"description":description,"inputSchema":{"type":"object","properties":properties}})).unwrap()).collect()
    }
    fn resolve(&self, name: &str) -> Result<Route, McpError> {
        let selected = self.selected();
        let mut candidates = Vec::new();
        for (cn, c) in &selected {
            if self.hidden(c) {
                continue;
            }
            for t in &c.discovery.read().unwrap().tools {
                if !c.spec.allows(&t.name) {
                    continue;
                }
                let duplicates = if self.connector.is_none()
                    && self.options.get("prefix").is_some_and(|p| p == "none")
                {
                    selected
                        .iter()
                        .filter(|(_, other)| !self.hidden(other))
                        .filter(|(_, other)| {
                            other
                                .discovery
                                .read()
                                .unwrap()
                                .tools
                                .iter()
                                .any(|other| other.name == t.name)
                        })
                        .count()
                } else {
                    0
                };
                let exported = if self.connector.is_none()
                    && (self.options.get("prefix").is_none_or(|p| p != "none") || duplicates > 1)
                {
                    format!("{cn}__{}", t.name)
                } else {
                    t.name.to_string()
                };
                if name == exported {
                    candidates.push(Route {
                        connector: cn.clone(),
                        upstream_name: t.name.to_string(),
                        tool: t.clone(),
                    });
                }
            }
        }
        if candidates.len() != 1 {
            return Err(McpError::invalid_params("unknown or ambiguous tool", None));
        }
        Ok(candidates.remove(0))
    }
    fn routes(&self) -> BTreeMap<String, Route> {
        let selected = self.selected();
        let mut counts = HashMap::new();
        for (_, c) in &selected {
            for t in &c.discovery.read().unwrap().tools {
                if c.spec.allows(&t.name) {
                    *counts.entry(t.name.to_string()).or_insert(0) += 1;
                }
            }
        }
        let mut routes = BTreeMap::new();
        for (name, c) in selected {
            if self.hidden(&c) {
                continue;
            }
            for t in &c.discovery.read().unwrap().tools {
                if !c.spec.allows(&t.name) {
                    continue;
                }
                let exported = if self.connector.is_none()
                    && (self.options.get("prefix").is_none_or(|p| p != "none")
                        || counts[t.name.as_ref()] > 1)
                {
                    format!("{name}__{}", t.name)
                } else {
                    t.name.to_string()
                };
                let mut value = serde_json::to_value(t).unwrap();
                for part in &c.spec.strip {
                    if let Some(m) = value.as_object_mut() {
                        m.remove(if part == "meta" { "_meta" } else { part });
                    }
                }
                value["name"] = serde_json::json!(exported);
                if let Ok(tool) = serde_json::from_value(value) {
                    routes.insert(
                        exported,
                        Route {
                            connector: name.clone(),
                            upstream_name: t.name.to_string(),
                            tool,
                        },
                    );
                }
            }
        }
        routes
    }
    fn single(&self, part: &str) -> Result<Arc<Connector>, McpError> {
        let selected = self.selected();
        if selected.len() != 1 || selected[0].1.spec.strips(part) {
            return Err(McpError::invalid_params("connector unavailable", None));
        }
        Ok(selected[0].1.clone())
    }
}
impl ServerHandler for Front {
    fn get_info(&self) -> ServerConfig {
        let mut info = ServerConfig::new(
            ServerCapabilities::builder()
                .enable_tools()
                .enable_prompts()
                .enable_resources()
                .build(),
        );
        if self.options.get("instructions").is_none_or(|v| v != "none") {
            info.instructions = Some(
                self.selected()
                    .iter()
                    .filter(|(_, c)| !c.spec.strips("instructions"))
                    .map(|(_, c)| c.discovery.read().unwrap().instructions.clone())
                    .collect::<Vec<_>>()
                    .join("\n\n"),
            );
        }
        info
    }
    async fn list_tools(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, McpError> {
        Ok(ListToolsResult {
            tools: self
                .routes()
                .into_values()
                .map(|r| r.tool)
                .chain(self.discovery_tools())
                .collect(),
            ..Default::default()
        })
    }
    async fn call_tool(
        &self,
        mut request: CallToolRequestParams,
        _: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, McpError> {
        let route = if self
            .discovery_tools()
            .iter()
            .any(|t| t.name == request.name)
        {
            let args = request.arguments.clone().unwrap_or_default();
            let name = args.get("tool").and_then(|v| v.as_str()).unwrap_or("");
            let cn = args.get("connector").and_then(|v| v.as_str()).unwrap_or("");
            let candidates: Vec<_> = self
                .selected()
                .into_iter()
                .filter(|(n, c)| self.hidden(c) && (cn.is_empty() || n == cn))
                .collect();
            if request.name == "search_tools" {
                let query = args
                    .get("query")
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .to_lowercase();
                let mut hits = Vec::new();
                for (n, c) in candidates {
                    for t in &c.discovery.read().unwrap().tools {
                        if c.spec.allows(&t.name)
                            && query.split_whitespace().all(|word| {
                                format!("{} {}", t.name, t.description.as_deref().unwrap_or(""))
                                    .to_lowercase()
                                    .contains(word)
                            })
                        {
                            hits.push(serde_json::json!({"connector":n,"tool":t.name,"description":t.description}));
                        }
                    }
                }
                return Ok(CallToolResult::success(vec![ContentBlock::text(
                    serde_json::to_string(&hits).map_err(internal)?,
                )])
                .into());
            }
            let mut found = Vec::new();
            for (n, c) in candidates {
                for t in &c.discovery.read().unwrap().tools {
                    if t.name == name && c.spec.allows(name) {
                        found.push(Route {
                            connector: n.clone(),
                            upstream_name: name.into(),
                            tool: t.clone(),
                        });
                    }
                }
            }
            if found.len() != 1 {
                return Err(McpError::invalid_params(
                    "unknown or ambiguous discovery tool",
                    None,
                ));
            }
            let route = found.remove(0);
            if request.name == "describe_tool" {
                return Ok(CallToolResult::success(vec![ContentBlock::text(
                    serde_json::to_string(&route.tool).map_err(internal)?,
                )])
                .into());
            }
            request.arguments = args.get("arguments").and_then(|v| v.as_object()).cloned();
            route
        } else {
            self.resolve(&request.name)?
        };
        let c = self
            .upstreams
            .read()
            .unwrap()
            .get(&route.connector)
            .cloned()
            .ok_or_else(|| internal("connector removed"))?;
        let id = uuid::Uuid::new_v4().to_string();
        self.receipt(id.clone(), route.clone(), "intent", "intent", 0)
            .await?;
        request.name = route.upstream_name.clone().into();
        let started = Instant::now();
        let result = async {
            let mut live = c.live.lock().await;
            c.ensure(&mut live).await.map_err(internal)?;
            let result = tokio::time::timeout(
                connectors::duration(&c.spec.call_timeout, 120).map_err(internal)?,
                live.client.as_ref().unwrap().call_tool(request),
            )
            .await
            .map_err(internal)?
            .map_err(internal);
            // Refresh after calls as well as list-change notifications (notification relay is still pending).
            if c.changed.swap(false, std::sync::atomic::Ordering::AcqRel) {
                let _ = c.discover(live.client.as_ref().unwrap()).await;
            }
            live.last_used = Instant::now();
            let result = result?;
            if c.spec.response.is_none() && !c.spec.strips("structured") {
                return Ok(result);
            }
            let mut value = serde_json::to_value(result).map_err(internal)?;
            if let Some(f) = &c.spec.response
                && (f.tools.is_empty() || connectors::matches(&f.tools, &route.upstream_name))
            {
                value = shaping::filter(value, f).await.map_err(internal)?;
            }
            if c.spec.strips("structured") {
                value.as_object_mut().unwrap().remove("structuredContent");
            }
            serde_json::from_value::<CallToolResult>(value).map_err(internal)
        }
        .await;
        let state = match &result {
            Ok(r) if r.is_error != Some(true) => "completed",
            _ => "failed",
        };
        self.receipt(
            id,
            route,
            "outcome",
            state,
            started.elapsed().as_micros() as i64,
        )
        .await?;
        result.map(Into::into)
    }
    async fn list_prompts(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListPromptsResult, McpError> {
        Ok(ListPromptsResult {
            prompts: self
                .selected()
                .iter()
                .filter(|(_, c)| !c.spec.strips("prompts"))
                .flat_map(|(_, c)| c.discovery.read().unwrap().prompts.clone())
                .collect(),
            ..Default::default()
        })
    }
    async fn list_resources(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListResourcesResult, McpError> {
        Ok(ListResourcesResult {
            resources: self
                .selected()
                .iter()
                .filter(|(_, c)| !c.spec.strips("resources"))
                .flat_map(|(_, c)| c.discovery.read().unwrap().resources.clone())
                .collect(),
            ..Default::default()
        })
    }
    async fn list_resource_templates(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListResourceTemplatesResult, McpError> {
        Ok(ListResourceTemplatesResult {
            resource_templates: self
                .selected()
                .iter()
                .filter(|(_, c)| !c.spec.strips("resources"))
                .flat_map(|(_, c)| c.discovery.read().unwrap().templates.clone())
                .collect(),
            ..Default::default()
        })
    }
    async fn get_prompt(
        &self,
        request: GetPromptRequestParams,
        _: RequestContext<RoleServer>,
    ) -> Result<GetPromptResponse, McpError> {
        let c = self.single("prompts")?;
        let mut live = c.live.lock().await;
        c.ensure(&mut live).await.map_err(internal)?;
        live.client
            .as_ref()
            .unwrap()
            .get_prompt(request)
            .await
            .map(Into::into)
            .map_err(internal)
    }
    async fn read_resource(
        &self,
        request: ReadResourceRequestParams,
        _: RequestContext<RoleServer>,
    ) -> Result<ReadResourceResponse, McpError> {
        let c = self.single("resources")?;
        let mut live = c.live.lock().await;
        c.ensure(&mut live).await.map_err(internal)?;
        live.client
            .as_ref()
            .unwrap()
            .read_resource(request)
            .await
            .map(Into::into)
            .map_err(internal)
    }
}
fn service(front: Front) -> StreamableHttpService<Front, LocalSessionManager> {
    StreamableHttpService::new(
        move || Ok(front.clone()),
        Arc::new(LocalSessionManager::default()),
        StreamableHttpServerConfig::default()
            .with_legacy_session_mode(false)
            .with_json_response(true),
    )
}

#[tokio::main(worker_threads = 2)]
async fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 4 {
        bail!("usage: brokerd-rust DATA_DIR OWNER_TOKEN_FILE PORT (loopback only)");
    }
    let root = PathBuf::from(&args[1]);
    let token = std::fs::read_to_string(&args[2])?.trim().to_string();
    if token.is_empty() {
        bail!("empty owner token");
    }
    let port: u16 = args[3].parse()?;
    if port == 8890 {
        bail!("prototype refuses live broker port 8890");
    }
    let upstreams: Registry = Arc::new(std::sync::RwLock::new(BTreeMap::new()));
    connectors::reload(&root, &upstreams).await?;
    let watch_root = root.clone();
    let watch_registry = upstreams.clone();
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(2));
        let mut hup =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::hangup()).unwrap();
        loop {
            tokio::select! {_=interval.tick()=>{},_=hup.recv()=>{}}
            if let Err(e) = connectors::reload(&watch_root, &watch_registry).await {
                eprintln!("reload rejected: {e}");
            }
            let connectors: Vec<_> = watch_registry.read().unwrap().values().cloned().collect();
            for c in connectors {
                let idle =
                    connectors::duration(&c.spec.idle_stop, if c.spec.on_demand { 600 } else { 0 })
                        .unwrap();
                if c.changed.load(std::sync::atomic::Ordering::Acquire)
                    && let Ok(live) = c.live.try_lock()
                    && let Some(client) = live.client.as_ref()
                    && c.discover(client).await.is_ok()
                {
                    c.changed.store(false, std::sync::atomic::Ordering::Release);
                }
                if !idle.is_zero()
                    && let Ok(mut live) = c.live.try_lock()
                    && live.last_used.elapsed() >= idle
                {
                    if let Some(task) = live.listener.take() {
                        task.abort();
                    }
                    live.client.take();
                }
            }
        }
    });
    let db = Connection::open(root.join("broker.db"))?;
    db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
      CREATE TABLE IF NOT EXISTS receipts(id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')), call_id TEXT NOT NULL, phase TEXT NOT NULL CHECK(phase IN ('intent','outcome')), connector TEXT NOT NULL, tool TEXT NOT NULL, state TEXT NOT NULL, duration_us INTEGER NOT NULL);
      CREATE INDEX IF NOT EXISTS receipts_call ON receipts(call_id);
      CREATE TRIGGER IF NOT EXISTS receipts_no_update BEFORE UPDATE ON receipts BEGIN SELECT RAISE(ABORT,'append-only'); END;
      CREATE TRIGGER IF NOT EXISTS receipts_no_delete BEFORE DELETE ON receipts BEGIN SELECT RAISE(ABORT,'append-only'); END;")?;
    let db = Arc::new(Mutex::new(db));
    let front = Front {
        upstreams: upstreams.clone(),
        connector: None,
        options: BTreeMap::new(),
        db,
    };
    use tower::ServiceExt;
    let mut app = Router::new().fallback(move |req: Request| {
        let mut front = front.clone();
        async move {
            let path = req.uri().path();
            if path != "/mcp" {
                let Some(name) = path.strip_prefix("/mcp/") else {
                    return Err(StatusCode::NOT_FOUND);
                };
                if !front.upstreams.read().unwrap().contains_key(name) {
                    return Err(StatusCode::NOT_FOUND);
                }
                front.connector = Some(name.into());
            }
            front.options = serde_urlencoded::from_str(req.uri().query().unwrap_or(""))
                .map_err(|_| StatusCode::BAD_REQUEST)?;
            Ok::<_, StatusCode>(service(front).oneshot(req).await.unwrap())
        }
    });
    let expected = format!("Bearer {token}");
    app = app.layer(middleware::from_fn(move |req: Request, next: Next| {
        let authorized = req
            .headers()
            .get("authorization")
            .is_some_and(|v| v.as_bytes() == expected.as_bytes());
        async move {
            if authorized {
                Ok::<Response, StatusCode>(next.run(req).await)
            } else {
                Err(StatusCode::UNAUTHORIZED)
            }
        }
    }));
    let listener = tokio::net::TcpListener::bind((std::net::Ipv4Addr::LOCALHOST, port)).await?;
    eprintln!("brokerd-rust ready on 127.0.0.1:{port}");
    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            let _ = tokio::signal::ctrl_c().await;
        })
        .await?;
    // Dropping RunningService and kill_on_drop transports terminates hosted children.
    drop(upstreams);
    Ok(())
}
