//! Comparison-only stdio MCP broker. No live-install defaults.
use anyhow::{Context, Result, bail};
use axum::{
    Router,
    extract::Request,
    http::StatusCode,
    middleware::{self, Next},
    response::Response,
};
use rmcp::{
    ErrorData as McpError, RoleClient, RoleServer, ServerHandler,
    model::*,
    service::{ClientLifecycleMode, ClientServiceExt, RequestContext, RunningService},
    transport::{
        TokioChildProcess,
        streamable_http_server::{
            StreamableHttpServerConfig, StreamableHttpService, session::local::LocalSessionManager,
        },
    },
};
use rusqlite::{Connection, params};
use serde::Deserialize;
use std::{
    collections::{BTreeMap, HashMap},
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use tokio::process::Command;

#[derive(Deserialize, Default)]
struct Spec {
    #[serde(default)]
    command: String,
    #[serde(default)]
    args: Vec<String>,
    #[serde(default)]
    env: BTreeMap<String, String>,
    url: Option<String>,
    oauth: Option<serde_json::Value>,
    #[serde(default)]
    disabled: bool,
}
#[derive(Deserialize)]
struct Config {
    connectors: BTreeMap<String, Spec>,
}
struct Upstream {
    client: RunningService<RoleClient, ()>,
    tools: Vec<Tool>,
}
#[derive(Clone)]
struct Route {
    connector: String,
    upstream_name: String,
    tool: Tool,
}
#[derive(Clone)]
struct Front {
    upstreams: Arc<BTreeMap<String, Upstream>>,
    routes: Arc<BTreeMap<String, Route>>,
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
impl ServerHandler for Front {
    fn get_info(&self) -> ServerConfig {
        ServerConfig::new(ServerCapabilities::builder().enable_tools().build())
    }
    async fn list_tools(
        &self,
        _: Option<PaginatedRequestParams>,
        _: RequestContext<RoleServer>,
    ) -> Result<ListToolsResult, McpError> {
        Ok(ListToolsResult {
            tools: self.routes.values().map(|r| r.tool.clone()).collect(),
            ..Default::default()
        })
    }
    async fn call_tool(
        &self,
        mut request: CallToolRequestParams,
        _: RequestContext<RoleServer>,
    ) -> Result<CallToolResponse, McpError> {
        let route = self
            .routes
            .get(request.name.as_ref())
            .ok_or_else(|| McpError::invalid_params("unknown tool", None))?
            .clone();
        let id = uuid::Uuid::new_v4().to_string();
        self.receipt(id.clone(), route.clone(), "intent", "intent", 0)
            .await?;
        request.name = route.upstream_name.clone().into();
        let start = Instant::now();
        let result = tokio::time::timeout(
            Duration::from_secs(120),
            self.upstreams[&route.connector].client.call_tool(request),
        )
        .await;
        let result = match result {
            Ok(r) => r.map_err(internal),
            Err(e) => Err(internal(e)),
        };
        let state = match &result {
            Ok(r) if r.is_error != Some(true) => "completed",
            _ => "failed",
        };
        self.receipt(
            id,
            route,
            "outcome",
            state,
            start.elapsed().as_micros() as i64,
        )
        .await?;
        result.map(Into::into)
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
    let config: Config = serde_yaml::from_slice(&std::fs::read(root.join("connectors.yaml"))?)?;
    let secrets: BTreeMap<String, String> = match std::fs::read(root.join("secrets.json")) {
        Ok(raw) => serde_json::from_slice(&raw)?,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => BTreeMap::new(),
        Err(e) => return Err(e.into()),
    };
    let refs = regex::Regex::new(r"\$\{secret:([A-Za-z0-9_]+)\}")?;
    let names = regex::Regex::new(r"^[a-z][a-z0-9_-]{0,63}$")?;
    let mut upstreams = BTreeMap::new();
    for (name, spec) in config.connectors {
        if spec.disabled || spec.url.is_some() || spec.oauth.is_some() {
            continue;
        }
        if !names.is_match(&name) || spec.command.is_empty() {
            bail!("invalid stdio connector {name}");
        }
        let mut cmd = Command::new(spec.command);
        cmd.args(spec.args).env_clear().kill_on_drop(true);
        for key in ["PATH", "HOME"] {
            if let Some(v) = std::env::var_os(key) {
                cmd.env(key, v);
            }
        }
        for (key, value) in spec.env {
            for cap in refs.captures_iter(&value) {
                if secrets.get(&cap[1]).is_none_or(|s| s.is_empty()) {
                    bail!("{name}: missing secret {}", &cap[1]);
                }
            }
            cmd.env(
                key,
                refs.replace_all(&value, |caps: &regex::Captures| secrets[&caps[1]].clone())
                    .as_ref(),
            );
        }
        let client = tokio::time::timeout(
            Duration::from_secs(90),
            ().serve_with_lifecycle(
                TokioChildProcess::new(cmd)?,
                ClientLifecycleMode::Auto {
                    preferred_versions: vec![ProtocolVersion::LATEST],
                    legacy_version: None,
                },
            ),
        )
        .await??;
        let tools = client
            .list_all_tools()
            .await
            .with_context(|| format!("discover {name}"))?;
        upstreams.insert(name, Upstream { client, tools });
    }
    let db = Connection::open(root.join("broker.db"))?;
    db.execute_batch("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
      CREATE TABLE IF NOT EXISTS receipts(id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')), call_id TEXT NOT NULL, phase TEXT NOT NULL CHECK(phase IN ('intent','outcome')), connector TEXT NOT NULL, tool TEXT NOT NULL, state TEXT NOT NULL, duration_us INTEGER NOT NULL);
      CREATE INDEX IF NOT EXISTS receipts_call ON receipts(call_id);
      CREATE TRIGGER IF NOT EXISTS receipts_no_update BEFORE UPDATE ON receipts BEGIN SELECT RAISE(ABORT,'append-only'); END;
      CREATE TRIGGER IF NOT EXISTS receipts_no_delete BEFORE DELETE ON receipts BEGIN SELECT RAISE(ABORT,'append-only'); END;")?;
    let db = Arc::new(Mutex::new(db));
    let upstreams = Arc::new(upstreams);
    let mut counts = HashMap::new();
    for up in upstreams.values() {
        for tool in &up.tools {
            *counts.entry(tool.name.to_string()).or_insert(0) += 1;
        }
    }
    let mut app = Router::new();
    let mut aggregate = BTreeMap::new();
    for (name, up) in upstreams.iter() {
        let mut routes = BTreeMap::new();
        for tool in &up.tools {
            let route = Route {
                connector: name.clone(),
                upstream_name: tool.name.to_string(),
                tool: tool.clone(),
            };
            routes.insert(tool.name.to_string(), route.clone());
            let exported = if counts[tool.name.as_ref()] > 1 {
                format!("{name}__{}", tool.name)
            } else {
                tool.name.to_string()
            };
            let mut route = route;
            route.tool.name = exported.clone().into();
            if aggregate.insert(exported.clone(), route).is_some() {
                bail!("aggregate tool name collision: {exported}");
            }
        }
        app = app.route_service(
            &format!("/mcp/{name}"),
            service(Front {
                upstreams: upstreams.clone(),
                routes: Arc::new(routes),
                db: db.clone(),
            }),
        );
    }
    app = app.route_service(
        "/mcp",
        service(Front {
            upstreams: upstreams.clone(),
            routes: Arc::new(aggregate),
            db,
        }),
    );
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
