package main

const loginPageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>AutoClaw 授权登录</title>
<style>
:root { color-scheme: light dark; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Microsoft YaHei", sans-serif; margin: 0; min-height: 100vh; display: grid; place-items: center; background: #0f1115; color: #e8eaed; }
.card { width: min(92vw, 420px); background: #171a21; border: 1px solid #2a2f3a; border-radius: 14px; padding: 28px; box-shadow: 0 20px 60px rgba(0,0,0,.35); }
h1 { font-size: 20px; margin: 0 0 6px; }
p.sub { margin: 0 0 22px; color: #9aa3b2; font-size: 13px; line-height: 1.6; }
label { display: block; font-size: 12px; color: #9aa3b2; margin: 14px 0 6px; }
input { width: 100%; box-sizing: border-box; background: #0f1115; color: #e8eaed; border: 1px solid #2a2f3a; border-radius: 9px; padding: 11px 12px; font-size: 15px; outline: none; }
input:focus { border-color: #5b8cff; }
.row { display: flex; gap: 8px; }
.row input { flex: 1; }
button { border: 0; border-radius: 9px; padding: 11px 14px; font-size: 14px; font-weight: 600; cursor: pointer; background: #5b8cff; color: #fff; }
button:disabled { opacity: .55; cursor: default; }
button.ghost { background: #232936; color: #cfd6e4; white-space: nowrap; }
#status { min-height: 20px; margin-top: 16px; font-size: 13px; line-height: 1.6; color: #9aa3b2; white-space: pre-wrap; }
#status.err { color: #ff7b72; }
#status.ok { color: #7ee787; }
.foot { margin-top: 20px; font-size: 12px; color: #697386; line-height: 1.6; }
</style>
</head>
<body>
<div class="card">
  <h1>AutoClaw 授权登录</h1>
  <p class="sub">用 AutoClaw 账号的手机号接收验证码。登录成功后，CPA 会自动保存账号并可以调用模型。验证码只会提交给 AutoClaw 官方身份网关。</p>
  <label for="phone">手机号</label>
  <input id="phone" autocomplete="tel" placeholder="例如 13800138000">
  <label for="code">验证码</label>
  <div class="row">
    <input id="code" inputmode="numeric" autocomplete="one-time-code" placeholder="6 位验证码">
    <button id="send" class="ghost" type="button">发送验证码</button>
  </div>
  <label for="alias">账号别名（可选）</label>
  <input id="alias" placeholder="default">
  <div style="margin-top:20px"><button id="submit" type="button" style="width:100%">登录并添加账号</button></div>
  <div id="status"></div>
  <div class="foot">授权页只监听 127.0.0.1。Docker 部署时请把 login_port 映射到宿主机同一端口。</div>
</div>
<script>
const state = "__STATE__";
const $ = (id) => document.getElementById(id);
const statusEl = $("status");
function setStatus(text, kind) {
  statusEl.textContent = text || "";
  statusEl.className = kind || "";
}
async function post(path, body) {
  const resp = await fetch(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
  let data = null;
  try { data = await resp.json(); } catch (e) {}
  if (!resp.ok || !data || data.ok !== true) {
    throw new Error((data && data.error) || ("HTTP " + resp.status));
  }
  return data;
}
$("send").addEventListener("click", async () => {
  const phone = $("phone").value.trim();
  if (!phone) { setStatus("请先填写手机号", "err"); return; }
  $("send").disabled = true;
  setStatus("正在发送验证码…");
  try {
    await post("/api/send-code", { state, phone });
    setStatus("验证码已发送，请查收短信。", "ok");
  } catch (e) {
    setStatus(e.message, "err");
  } finally {
    $("send").disabled = false;
  }
});
$("submit").addEventListener("click", async () => {
  const phone = $("phone").value.trim();
  const code = $("code").value.trim();
  const alias = $("alias").value.trim();
  if (!phone || !code) { setStatus("请填写手机号和验证码", "err"); return; }
  $("submit").disabled = true;
  setStatus("正在登录…");
  try {
    const data = await post("/api/login", { state, phone, code, alias });
    setStatus("登录成功：" + (data.alias || "账号已添加") + "。可以关闭此页面，回到 CPA 继续。", "ok");
  } catch (e) {
    setStatus(e.message, "err");
    $("submit").disabled = false;
  }
});
</script>
</body>
</html>
`
