# 项目长期记忆

## 环境

- Go 必须用 `/opt/homebrew/bin/go`（1.27.x）；`/usr/local/go/bin/go` 是 1.20，缺 `http.FileServerFS` / `r.PathValue`，编译本项目会失败。`make` 前先 `PATH=/opt/homebrew/bin:$PATH`。

## 配置分层约定

- 发件凭据（SMTP_SERVER/PORT/USER/PASS/SSL）只在 `.env`，改后需重启。
- 收件人可在 Web 面板改，存 `dataDir/mail.json`（0600），不写回 `.env`；Mailer 每封邮件读最新值，无需重启。
- Web 面板无鉴权，仅绑回环；所有写操作必须带 X-BW 头 + JSON Content-Type。
