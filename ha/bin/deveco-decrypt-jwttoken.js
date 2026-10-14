const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const configPath = "C:/Users/Administrator/.config/deveco";
const keyDir = path.join(configPath, "keys");
function unwrapDek(wrapped) {
  const kek = fs.readFileSync(path.join(keyDir, wrapped.kekId + ".bin"));
  const iv = Buffer.from(wrapped.iv, "base64");
  const authTag = Buffer.from(wrapped.authTag, "base64");
  const encryptedDek = Buffer.from(wrapped.encryptedDek, "base64");
  const d = crypto.createDecipheriv("aes-256-gcm", kek, iv);
  d.setAuthTag(authTag);
  return Buffer.concat([d.update(encryptedDek), d.final()]);
}
const dek = unwrapDek(JSON.parse(fs.readFileSync(path.join(configPath, "token.dek"), "utf8")));
function decrypt(blob) {
  const iv = Buffer.from(blob.iv, "base64");
  const authTag = Buffer.from(blob.authTag, "base64");
  const ct = Buffer.from(blob.ciphertext, "base64");
  const d = crypto.createDecipheriv("aes-256-gcm", dek, iv);
  d.setAuthTag(authTag);
  return Buffer.concat([d.update(ct), d.final()]).toString("utf8");
}
const blob = JSON.parse(fs.readFileSync(path.join(configPath, "token.enc"), "utf8"));
const jwt = decrypt(blob);
console.log("JWTTOKEN_START");
console.log(jwt);
console.log("JWTTOKEN_END");
const payload = JSON.parse(Buffer.from(jwt.split(".")[1].replace(/-/g, "+").replace(/_/g, "/"), "base64").toString("utf8"));
console.log("PAYLOAD: " + JSON.stringify(payload));
