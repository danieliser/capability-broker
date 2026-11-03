use anyhow::{Result, bail};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use tokio::io::AsyncWriteExt;
#[derive(Clone, Deserialize, Serialize, Default, PartialEq)]
#[serde(default)]
pub struct Filter {
    pub drop_keys: Vec<String>,
    pub compact: bool,
    pub max_string: usize,
    pub max_items: usize,
    pub command: Vec<String>,
    pub max_chars: usize,
    pub tools: Vec<String>,
}
fn reshape(v: Value, f: &Filter, depth: usize) -> Value {
    match v {
        Value::Object(m) => {
            if f.compact && depth >= 6 {
                return json!(format!("{{object with {} keys}}", m.len()));
            }
            Value::Object(
                m.into_iter()
                    .filter(|(k, _)| !f.drop_keys.contains(k))
                    .map(|(k, v)| (k, reshape(v, f, depth + 1)))
                    .collect(),
            )
        }
        Value::Array(a) => {
            if f.compact && depth >= 6 {
                return json!(format!("[array of {}]", a.len()));
            }
            let n = a.len();
            let max = if f.compact {
                if f.max_items == 0 { 50 } else { f.max_items }
            } else {
                n
            };
            let mut out: Vec<_> = a
                .into_iter()
                .take(max)
                .map(|v| reshape(v, f, depth + 1))
                .collect();
            if n > max {
                out.push(json!(format!("… [{} more items]", n - max)));
            }
            Value::Array(out)
        }
        Value::String(s) if f.compact => {
            let max = if f.max_string == 0 { 400 } else { f.max_string };
            let n = s.chars().count();
            if n > max {
                json!(format!(
                    "{}… [{} more chars]",
                    s.chars().take(max).collect::<String>(),
                    n - max
                ))
            } else {
                json!(s)
            }
        }
        v => v,
    }
}
pub async fn filter(mut v: Value, f: &Filter) -> Result<Value> {
    if !f.drop_keys.is_empty() || f.compact {
        if let Some(content) = v["content"].as_array_mut() {
            for c in content {
                if let Some(text) = c["text"].as_str()
                    && let Ok(data) = serde_json::from_str(text)
                {
                    c["text"] = json!(serde_json::to_string(&reshape(data, f, 0))?);
                }
            }
        }
        if let Some(sc) = v.get_mut("structuredContent") {
            *sc = reshape(sc.take(), f, 0);
        }
    }
    if let Some(cmd) = f.command.first() {
        let mut child = tokio::process::Command::new(cmd)
            .args(&f.command[1..])
            .stdin(std::process::Stdio::piped())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::piped())
            .kill_on_drop(true)
            .spawn()?;
        let mut stdin = child.stdin.take().unwrap();
        let input = serde_json::to_vec(&v)?;
        let writer = tokio::spawn(async move { stdin.write_all(&input).await });
        let output =
            tokio::time::timeout(std::time::Duration::from_secs(30), child.wait_with_output())
                .await??;
        writer.await??;
        if !output.status.success() {
            bail!("response command failed");
        }
        let text = String::from_utf8(output.stdout)?.trim().to_string();
        v=serde_json::from_str::<Value>(&text).ok().filter(|v|v["content"].is_array()).unwrap_or_else(||json!({"content":[{"type":"text","text":text}],"isError":v["isError"].as_bool().unwrap_or(false)}));
    }
    if f.max_chars > 0
        && let Some(a) = v["content"].as_array_mut()
    {
        for c in a {
            if let Some(s) = c["text"].as_str() {
                let n = s.chars().count();
                if n > f.max_chars {
                    c["text"] = json!(format!(
                        "{}\n…[{} more characters cut by the broker]",
                        s.chars().take(f.max_chars).collect::<String>(),
                        n - f.max_chars
                    ));
                }
            }
        }
    }
    Ok(v)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[tokio::test]
    async fn recursive_keys_and_unicode() {
        let f = Filter {
            drop_keys: vec!["debug".into()],
            max_chars: 2,
            ..Default::default()
        };
        let v=filter(json!({"content":[{"type":"text","text":"é😀x"}],"structuredContent":{"debug":0,"a":[{"debug":1,"ok":2}]}}),&f).await.unwrap();
        assert_eq!(v["structuredContent"], json!({"a":[{"ok":2}]}));
        assert!(
            v["content"][0]["text"]
                .as_str()
                .unwrap()
                .starts_with("é😀\n")
        );
    }
}
