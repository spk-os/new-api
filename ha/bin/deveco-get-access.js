// deveco accessToken 获取（在 Windows 上运行）
// 解密 token.enc 中的 jwtToken -> GET jwToken/check 换新鲜 accessToken -> stdout 最后一行输出
// 背景：新登录的 jwtToken 被华为风控绑定登录环境（仅 Windows+node 可过 check），accessToken 无绑定。
// 多账号：deveco CLI 支持 DEVECO_CONFIG_DIR 独立配置目录，每个账号一个目录（互不覆盖），
//   例：C:/Users/Administrator/.deveco-9227、C:/Users/Administrator/.deveco-6506。
// 用法: node deveco-get-access.js [configDir] [expectedUserId]
//   configDir       可选，默认 C:/Users/Administrator/.config/deveco（CLI 默认目录）
//   expectedUserId  可选，校验 token.enc 归属账号，不匹配则报 ACCOUNT_MISMATCH 并退出——
//                   防止目录里是别的账号的 token，cron 把错误的 accessToken 写进渠道。
const fs = require("fs"), path = require("path"), crypto = require("crypto");
const DEFAULT_CONFIG = "C:/Users/Administrator/.config/deveco";
const args = process.argv.slice(2).filter((a) => a !== "");
let configPath = DEFAULT_CONFIG;
let expectedUserId = "";
// 参数判别：含路径分隔符的是 configDir，纯数字是 expectedUserId（兼容旧的单参数调用）
for (const a of args) {
  if (/[\/\\]/.test(a)) {
    configPath = a;
  } else if (/^\d+$/.test(a)) {
    expectedUserId = a;
  }
}
const wrapped = JSON.parse(fs.readFileSync(path.join(configPath, "token.dek"), "utf8"));
const kek = fs.readFileSync(path.join(configPath, "keys", wrapped.kekId + ".bin"));
const d1 = crypto.createDecipheriv("aes-256-gcm", kek, Buffer.from(wrapped.iv, "base64"));
d1.setAuthTag(Buffer.from(wrapped.authTag, "base64"));
const dek = Buffer.concat([d1.update(Buffer.from(wrapped.encryptedDek, "base64")), d1.final()]);
const blob = JSON.parse(fs.readFileSync(path.join(configPath, "token.enc"), "utf8"));
const d2 = crypto.createDecipheriv("aes-256-gcm", dek, Buffer.from(blob.iv, "base64"));
d2.setAuthTag(Buffer.from(blob.authTag, "base64"));
const jwt = Buffer.concat([d2.update(Buffer.from(blob.ciphertext, "base64")), d2.final()]).toString("utf8");
let payload = {};
try {
  payload = JSON.parse(Buffer.from(jwt.split(".")[1].replace(/-/g, "+").replace(/_/g, "/"), "base64").toString("utf8"));
} catch (e) { /* payload 解析失败继续，check 会兜底 */ }
if (expectedUserId && String(payload.userId || "") !== expectedUserId) {
  console.log("ACCOUNT_MISMATCH expected=" + expectedUserId + " actual=" + payload.userId);
  process.exit(2);
}
(async () => {
  const r = await fetch("https://cn.devecostudio.huawei.com/authrouter/auth/api/jwToken/check", {
    headers: { "refresh": "true", "jwtToken": jwt },
  });
  const j = await r.json();
  if (!j.status || !j.userInfo || !j.userInfo.accessToken) {
    console.log("CHECK_FALSE");
    process.exit(1);
  }
  console.log(j.userInfo.accessToken);
})();
