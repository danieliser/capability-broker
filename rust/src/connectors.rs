use anyhow::{Result, bail};
use rmcp::{
    ClientHandler, RoleClient,
    model::*,
    service::{ClientLifecycleMode, ClientServiceExt, NotificationContext, RunningService},
    transport::{
        StreamableHttpClientTransport, TokioChildProcess,
        streamable_http_client::StreamableHttpClientTransportConfig,
    },
};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeMap,
    path::{Path, PathBuf},
    sync::{Arc, RwLock},
    time::{Duration, Instant},
};
use tokio::{process::Command, sync::Mutex};

#[derive(Clone, Deserialize, Serialize, Default, PartialEq)]
#[serde(default)]
pub struct Spec {
    pub command: String,
    pub args: Vec<String>,
    pub env: BTreeMap<String, String>,
    pub url: String,
    pub headers: BTreeMap<String, String>,
    pub oauth: Option<serde_json::Value>,
    pub cwd: String,
    pub disabled: bool,
    pub on_demand: bool,
    pub idle_stop: String,
    pub startup_timeout: String,
    pub call_timeout: String,
    pub pin: Option<bool>,
    pub owner_only: bool,
    pub clients: Vec<String>,
    pub expose: String,
    pub tools: ToolFilter,
    pub strip: Vec<String>,
    pub response: Option<crate::shaping::Filter>,
}
#[derive(Clone, Deserialize, Serialize, Default, PartialEq)]
#[serde(default)]
pub struct ToolFilter {
    pub include: Vec<String>,
    pub exclude: Vec<String>,
}
pub fn matches(patterns: &[String], name: &str) -> bool {
    patterns.iter().any(|p| {
        glob::Pattern::new(p).is_ok_and(|g| {
            g.matches_with(
                name,
                glob::MatchOptions {
                    require_literal_separator: true,
                    ..Default::default()
                },
            )
        })
    })
}
impl Spec {
    pub fn allows(&self, name: &str) -> bool {
        !matches(&self.tools.exclude, name)
            && (self.tools.include.is_empty() || matches(&self.tools.include, name))
    }
    pub fn strips(&self, part: &str) -> bool {
        self.strip.iter().any(|p| p == part)
    }
}
// Go duration strings, including composite values such as 1m30s.
pub fn duration(s: &str, default: u64) -> Result<Duration> {
    if s.is_empty() || s == "0" {
        return Ok(Duration::from_secs(default));
    }
    let re = regex::Regex::new(r"([0-9]+(?:\.[0-9]+)?)(ns|us|µs|ms|s|m|h)")?;
    let mut end = 0;
    let mut seconds = 0.;
    for c in re.captures_iter(s) {
        let m = c.get(0).unwrap();
        if m.start() != end {
            bail!("invalid duration {s}");
        }
        end = m.end();
        let scale = match &c[2] {
            "h" => 3600.,
            "m" => 60.,
            "s" => 1.,
            "ms" => 0.001,
            "us" | "µs" => 0.000001,
            _ => 0.000000001,
        };
        seconds += c[1].parse::<f64>()? * scale;
    }
    if end != s.len() || seconds <= 0. || !seconds.is_finite() {
        bail!("invalid duration {s}");
    }
    Ok(Duration::from_secs_f64(seconds))
}
fn home(s: &str) -> String {
    s.strip_prefix("~/")
        .map(|p| format!("{}/{p}", std::env::var("HOME").unwrap_or_default()))
        .unwrap_or_else(|| s.into())
}
pub fn load(root: &Path) -> Result<BTreeMap<String, Spec>> {
    #[derive(Deserialize, Default)]
    struct Config {
        #[serde(default)]
        connectors: BTreeMap<String, Option<Spec>>,
    }
    use std::os::unix::fs::PermissionsExt;
    if let Ok(meta) = std::fs::metadata(root.join("secrets.json"))
        && meta.permissions().mode() & 0o077 != 0
    {
        bail!("secrets.json must have mode 0600");
    }
    let secrets: BTreeMap<String, String> = match std::fs::read(root.join("secrets.json")) {
        Ok(b) => serde_json::from_slice(&b)?,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => BTreeMap::new(),
        Err(e) => return Err(e.into()),
    };
    let config: Config = match std::fs::read(root.join("connectors.yaml")) {
        Ok(b) => serde_yaml::from_slice(&b)?,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Config::default(),
        Err(e) => return Err(e.into()),
    };
    let names = regex::Regex::new(r"^[a-z][a-z0-9_-]{0,63}$")?;
    let refs = regex::Regex::new(r"\$\{(secret|env):([A-Za-z0-9_]+)\}")?;
    let resolve = |v: &str| -> Result<String> {
        for c in refs.captures_iter(v) {
            if &c[1] == "secret" && secrets.get(&c[2]).is_none_or(|x| x.is_empty()) {
                bail!("missing secret {}", &c[2]);
            }
        }
        Ok(home(&refs.replace_all(v, |c: &regex::Captures| {
            if &c[1] == "secret" {
                secrets[&c[2]].clone()
            } else {
                std::env::var(&c[2]).unwrap_or_default()
            }
        })))
    };
    let mut out = BTreeMap::new();
    for (name, spec) in config.connectors {
        if !names.is_match(&name) || name == "persist" {
            bail!("invalid connector name {name}");
        }
        let Some(mut s) = spec else { continue };
        if s.disabled {
            continue;
        }
        if s.command.trim().is_empty() == s.url.trim().is_empty() {
            bail!("{name}: exactly one of command or url required");
        }
        if !["", "broker", "passthrough"].contains(&s.expose.as_str()) {
            bail!("{name}: invalid expose");
        }
        if !s.url.is_empty() {
            let url = reqwest::Url::parse(&s.url)?;
            if url.scheme() != "https"
                && !(url.scheme() == "http"
                    && [Some("127.0.0.1"), Some("localhost")].contains(&url.host_str()))
            {
                bail!("{name}: HTTPS required except loopback");
            }
        }
        s.command = home(&s.command);
        s.cwd = home(&s.cwd);
        for map in [&mut s.env, &mut s.headers] {
            for value in map.values_mut() {
                *value = resolve(value)?;
            }
        }
        duration(&s.startup_timeout, 90)?;
        duration(&s.call_timeout, 120)?;
        duration(&s.idle_stop, if s.on_demand { 600 } else { 0 })?;
        // Never silently grant an unsupported OAuth connector unauthenticated access.
        if s.oauth.is_some() {
            bail!("{name}: OAuth parity is not implemented");
        }
        out.insert(name, s);
    }
    Ok(out)
}
#[derive(Clone, Default, Deserialize, Serialize)]
#[serde(default)]
pub struct Discovery {
    pub instructions: String,
    pub tools: Vec<Tool>,
    pub prompts: Vec<Prompt>,
    pub resources: Vec<Resource>,
    pub templates: Vec<ResourceTemplate>,
}
#[derive(Clone)]
pub struct Changes(Arc<std::sync::atomic::AtomicBool>);
impl ClientHandler for Changes {
    async fn on_tool_list_changed(&self, _: NotificationContext<RoleClient>) {
        self.0.store(true, std::sync::atomic::Ordering::Release);
    }
    async fn on_prompt_list_changed(&self, _: NotificationContext<RoleClient>) {
        self.0.store(true, std::sync::atomic::Ordering::Release);
    }
    async fn on_resource_list_changed(&self, _: NotificationContext<RoleClient>) {
        self.0.store(true, std::sync::atomic::Ordering::Release);
    }
}
pub struct Live {
    pub listener: Option<tokio::task::JoinHandle<()>>,
    pub client: Option<RunningService<RoleClient, Changes>>,
    pub last_used: Instant,
}
impl Drop for Live {
    fn drop(&mut self) {
        if let Some(task) = self.listener.take() {
            task.abort();
        }
    }
}
pub struct Connector {
    pub spec: Spec,
    pub discovery: RwLock<Discovery>,
    pub live: Mutex<Live>,
    cache: PathBuf,
    pub changed: Arc<std::sync::atomic::AtomicBool>,
}
impl Connector {
    pub fn new(root: &Path, name: &str, spec: Spec) -> Self {
        let cache = root.join("cache").join(format!("{name}.json"));
        let discovery = if spec.on_demand {
            std::fs::read(&cache)
                .ok()
                .and_then(|b| serde_json::from_slice(&b).ok())
                .unwrap_or_default()
        } else {
            Discovery::default()
        };
        Self {
            spec,
            discovery: RwLock::new(discovery),
            live: Mutex::new(Live {
                client: None,
                listener: None,
                last_used: Instant::now(),
            }),
            cache,
            changed: Arc::new(std::sync::atomic::AtomicBool::new(false)),
        }
    }
    pub async fn ensure(&self, live: &mut Live) -> Result<()> {
        live.last_used = Instant::now();
        if live.client.is_some() {
            return Ok(());
        }
        let lifecycle = ClientLifecycleMode::Auto {
            preferred_versions: vec![ProtocolVersion::LATEST],
            legacy_version: None,
        };
        let connect = async {
            if !self.spec.url.is_empty() {
                let mut config =
                    StreamableHttpClientTransportConfig::with_uri(self.spec.url.clone());
                for (k, v) in &self.spec.headers {
                    config.custom_headers.insert(k.parse()?, v.parse()?);
                }
                config
                    .custom_headers
                    .insert("mcp-protocol-version".parse()?, "2025-11-25".parse()?);
                let http = reqwest::Client::builder()
                    .http1_only()
                    .user_agent("brokerd/0.10.0")
                    .build()?;
                Ok::<_, anyhow::Error>(
                    Changes(self.changed.clone())
                        .serve_with_lifecycle(
                            StreamableHttpClientTransport::with_client(http, config),
                            lifecycle,
                        )
                        .await?,
                )
            } else {
                let mut cmd = Command::new(&self.spec.command);
                cmd.args(&self.spec.args).env_clear().kill_on_drop(true);
                for key in ["PATH", "HOME"] {
                    if let Some(v) = std::env::var_os(key) {
                        cmd.env(key, v);
                    }
                }
                cmd.envs(&self.spec.env);
                if !self.spec.cwd.is_empty() {
                    cmd.current_dir(&self.spec.cwd);
                }
                Ok(Changes(self.changed.clone())
                    .serve_with_lifecycle(TokioChildProcess::new(cmd)?, lifecycle)
                    .await?)
            }
        };
        let client =
            tokio::time::timeout(duration(&self.spec.startup_timeout, 90)?, connect).await??;
        self.discover(&client).await?;
        if client
            .peer_info()
            .is_some_and(|i| i.protocol_version == ProtocolVersion::LATEST)
        {
            let filter = SubscriptionFilter::builder()
                .tools_list_changed()
                .prompts_list_changed()
                .resources_list_changed()
                .build();
            let mut subscription =
                tokio::time::timeout(Duration::from_secs(5), client.listen(filter)).await??;
            let changed = self.changed.clone();
            live.listener = Some(tokio::spawn(async move {
                while let Ok(Some(_)) = subscription.next().await {
                    changed.store(true, std::sync::atomic::Ordering::Release);
                }
            }));
        }
        live.client = Some(client);
        Ok(())
    }
    pub async fn discover(&self, client: &RunningService<RoleClient, Changes>) -> Result<()> {
        let info = client.peer_info();
        let mut d = Discovery {
            tools: client.list_all_tools().await?,
            instructions: info
                .as_ref()
                .and_then(|i| i.instructions.clone())
                .unwrap_or_default(),
            ..Default::default()
        };
        if info
            .as_ref()
            .is_some_and(|i| i.capabilities.prompts.is_some())
        {
            d.prompts = client.list_all_prompts().await?;
        }
        if info
            .as_ref()
            .is_some_and(|i| i.capabilities.resources.is_some())
        {
            d.resources = client.list_all_resources().await?;
            d.templates = client.list_all_resource_templates().await?;
        }
        if self.spec.on_demand || !self.spec.idle_stop.is_empty() {
            std::fs::create_dir_all(self.cache.parent().unwrap())?;
            std::fs::set_permissions(
                self.cache.parent().unwrap(),
                std::os::unix::fs::PermissionsExt::from_mode(0o700),
            )?;
            use std::io::Write;
            use std::os::unix::fs::OpenOptionsExt;
            let tmp = self.cache.with_extension("json.tmp");
            let mut f = std::fs::OpenOptions::new()
                .write(true)
                .create(true)
                .truncate(true)
                .mode(0o600)
                .open(&tmp)?;
            f.write_all(&serde_json::to_vec(&d)?)?;
            std::fs::rename(tmp, &self.cache)?;
        }
        *self.discovery.write().unwrap() = d;
        Ok(())
    }
}
pub type Registry = Arc<RwLock<BTreeMap<String, Arc<Connector>>>>;
pub async fn reload(root: &Path, registry: &Registry) -> Result<()> {
    let specs = load(root)?; // Parse the whole configuration before changing anything.
    let old = registry.read().unwrap().clone();
    let mut next = BTreeMap::new();
    for (name, spec) in specs {
        if let Some(c) = old.get(&name).filter(|c| c.spec == spec) {
            next.insert(name, c.clone());
            continue;
        }
        let c = Arc::new(Connector::new(root, &name, spec));
        if (!c.spec.on_demand || c.discovery.read().unwrap().tools.is_empty())
            && let Err(e) = c.ensure(&mut *c.live.lock().await).await
        {
            eprintln!("connector {name} failed: {e}");
        }
        next.insert(name, c);
    }
    *registry.write().unwrap() = next;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn durations_and_filters() {
        assert_eq!(duration("1m30s", 0).unwrap(), Duration::from_secs(90));
        assert_eq!(duration("250ms", 0).unwrap(), Duration::from_millis(250));
        assert!(duration("1mBAD", 0).is_err());
        assert!(duration("-1s", 0).is_err());
        let s = Spec {
            tools: ToolFilter {
                include: vec!["read*".into()],
                exclude: vec!["read_secret".into()],
            },
            ..Default::default()
        };
        assert!(s.allows("read_all"));
        assert!(!s.allows("read_secret"));
        assert!(!s.allows("write"));
    }
    #[test]
    fn invalid_reload_is_atomic() {
        let root = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join(".bench")
            .join(format!("unit-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&root).unwrap();
        std::fs::write(root.join("connectors.yaml"),"connectors:\n  good: {command: echo}\n  bad: {command: echo, url: 'https://example.invalid'}\n").unwrap();
        assert!(load(&root).is_err());
        std::fs::write(
            root.join("connectors.yaml"),
            "connectors:\n  good: {command: echo, env: {KEY: '${secret:MISSING}'}}\n",
        )
        .unwrap();
        assert!(load(&root).is_err());
        std::fs::write(
            root.join("connectors.yaml"),
            "connectors:\n  good: {url: 'http://localhost.evil.invalid/mcp'}\n",
        )
        .unwrap();
        assert!(load(&root).is_err());
        std::fs::remove_dir_all(root).unwrap();
    }
}
