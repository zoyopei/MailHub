import argparse
import os
import tempfile
from pathlib import Path

import requests
from requests.adapters import HTTPAdapter
from urllib3.util.retry import Retry
from imap_tools import MailBox, MailboxLoginError, AND


GRAPH_URL = "https://graph.microsoft.com/v1.0"
TOKEN_URL = "https://login.microsoftonline.com/common/oauth2/v2.0/token"
IMAP_HOST = "outlook.office365.com"

# 两种协议对应不同 audience 的访问令牌，不能混用。
GRAPH_SCOPE = "https://graph.microsoft.com/.default offline_access"
IMAP_SCOPE = "https://outlook.office.com/IMAP.AccessAsUser.All offline_access"


class MailError(Exception):
    pass


def load_accounts(path):
    """保留原始行，方便在指定选项时更新刷新令牌。"""
    lines = path.read_text(encoding="utf-8-sig").splitlines()
    accounts = []

    for index, line in enumerate(lines):
        if not line.strip() or line.lstrip().startswith("#"):
            continue

        parts = [part.strip() for part in line.split("----", 3)]
        if len(parts) != 4 or not all(parts[i] for i in (0, 2, 3)):
            raise MailError(
                f"账号文件第 {index + 1} 行格式错误："
                "应为 邮箱----密码----client_id----refresh_token"
            )

        accounts.append((index, parts))

    return lines, accounts


def save_lines(path, lines):
    """通过同目录临时文件替换，避免直接覆盖导致文件损坏。"""
    temp_path = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="w",
            encoding="utf-8",
            dir=str(path.parent),
            prefix=f".{path.name}.",
            delete=False,
        ) as file:
            temp_path = Path(file.name)
            file.write("\n".join(lines) + "\n")
            file.flush()
            os.fsync(file.fileno())

        # 在支持 POSIX 权限的平台上，仅允许当前用户读写。
        if os.name == "posix":
            os.chmod(temp_path, 0o600)

        os.replace(temp_path, path)
    finally:
        if temp_path is not None and temp_path.exists():
            temp_path.unlink()


def create_session():
    session = requests.Session()
    # 邮件读取遇到限流或临时服务故障时重试；不自动重试 POST。
    retries = Retry(
        total=3,
        backoff_factor=1,
        status_forcelist=[429, 500, 502, 503, 504],
        allowed_methods=["GET"],
        respect_retry_after_header=True,
    )
    session.mount("https://", HTTPAdapter(max_retries=retries))
    return session


def refresh_access_token(session, client_id, refresh_token, scope):
    response = session.post(
        TOKEN_URL,
        data={
            "client_id": client_id,
            "grant_type": "refresh_token",
            "refresh_token": refresh_token,
            "scope": scope,
        },
        timeout=(10, 30),
    )

    try:
        data = response.json()
    except ValueError:
        raise MailError(f"令牌接口返回异常：HTTP {response.status_code}")

    if response.status_code != 200:
        # 不打印完整响应，避免意外暴露敏感信息。
        code = data.get("error", "unknown_error")
        if code == "invalid_grant":
            raise MailError(
                "刷新令牌无效或已过期（invalid_grant），该账号需要重新授权。"
            )
        raise MailError(
            f"获取访问令牌失败：{code}。"
            "请检查 client_id、刷新令牌、应用类型和授权权限。"
        )

    if not data.get("access_token"):
        raise MailError("令牌接口未返回 access_token。")

    return data["access_token"], data.get("refresh_token")


def fetch_via_graph(session, access_token, limit, unread_only):
    """通过 Graph REST API 读取，返回统一的邮件摘要列表。"""
    params = {
        "$top": limit,
        "$select": "id,subject,from,receivedDateTime,isRead,bodyPreview",
        "$orderby": "receivedDateTime desc",
    }
    if unread_only:
        params["$filter"] = "isRead eq false"

    response = session.get(
        f"{GRAPH_URL}/me/mailFolders/inbox/messages",
        headers={"Authorization": f"Bearer {access_token}"},
        params=params,
        timeout=(10, 30),
    )

    if response.status_code != 200:
        hints = {
            401: "访问令牌无效，或该令牌不是用于 Microsoft Graph 的。",
            403: "缺少 Mail.Read 权限，或邮箱访问受组织策略限制。",
            404: "未找到邮箱；请确认该账号已开通 Outlook/Exchange 邮箱。",
        }
        hint = hints.get(response.status_code, "请检查邮箱和应用配置。")
        raise MailError(f"读取邮件失败：HTTP {response.status_code}；{hint}")

    try:
        value = response.json().get("value", [])
    except ValueError:
        raise MailError("邮件接口返回了无法解析的数据。")

    summaries = []
    for message in value:
        sender = (message.get("from") or {}).get("emailAddress") or {}
        summaries.append({
            "subject": message.get("subject") or "(无主题)",
            "name": sender.get("name", ""),
            "address": sender.get("address", ""),
            "time": message.get("receivedDateTime", ""),
            "unread": not message.get("isRead"),
            "preview": message.get("bodyPreview", ""),
        })
    return summaries


def fetch_via_imap(email_address, access_token, limit, unread_only):
    """通过 IMAP XOAUTH2 读取，返回与 Graph 路径一致的邮件摘要列表。"""
    criteria = AND(seen=False) if unread_only else "ALL"
    summaries = []

    try:
        with MailBox(IMAP_HOST).xoauth2(email_address, access_token) as mailbox:
            # mark_seen=False：读取预览不改变邮件的已读状态。
            for message in mailbox.fetch(
                criteria=criteria, limit=limit, mark_seen=False, reverse=True
            ):
                preview = " ".join((message.text or "").split())[:200]
                summaries.append({
                    "subject": message.subject or "(无主题)",
                    "name": message.from_values.name if message.from_values else "",
                    "address": message.from_values.email if message.from_values else "",
                    "time": str(message.date or ""),
                    "unread": "\\Seen" not in message.flags,
                    "preview": preview,
                })
    except MailboxLoginError:
        raise MailError(
            "IMAP 认证失败：XOAUTH2 被服务器拒绝。"
            "请确认账号已启用 IMAP，且令牌含 IMAP.AccessAsUser.All 权限。"
        )

    return summaries


def print_messages(messages):
    if not messages:
        print("没有符合条件的邮件。")
        return

    for index, message in enumerate(messages, 1):
        state = "未读" if message["unread"] else "已读"

        print(f"\n[{index}] {message['subject']}")
        print(f"发件人：{message['name']} <{message['address']}>")
        print(f"接收时间：{message['time']}")
        print(f"状态：{state}")
        print(f"正文预览：{message['preview']}")
        print("-" * 60)


def main():
    parser = argparse.ArgumentParser(description="批量读取 Outlook 收件箱（Graph API / IMAP）")
    parser.add_argument("--file", default="accounts.txt", help="账号文件路径")
    parser.add_argument("--limit", type=int, default=10, help="每个邮箱读取数量，1～100")
    parser.add_argument("--unread", action="store_true", help="只读取未读邮件")
    parser.add_argument(
        "--protocol",
        choices=["graph", "imap"],
        default="graph",
        help="读取协议：graph = Graph REST API（默认），imap = IMAP XOAUTH2",
    )
    parser.add_argument(
        "--save-token",
        action="store_true",
        help="将服务端返回的新刷新令牌更新到账号文件",
    )
    args = parser.parse_args()

    if not 1 <= args.limit <= 100:
        parser.error("--limit 必须在 1～100 之间")

    path = Path(args.file).resolve()
    try:
        lines, accounts = load_accounts(path)
    except (OSError, UnicodeError, MailError) as error:
        print(f"无法读取账号文件：{error}")
        return

    if not accounts:
        print("账号文件中没有有效账号。")
        return

    scope = GRAPH_SCOPE if args.protocol == "graph" else IMAP_SCOPE

    with create_session() as session:
        for line_index, parts in accounts:
            email_address, password, client_id, refresh_token = parts
            print(f"\n{'=' * 60}\n邮箱：{email_address}（{args.protocol}）")

            try:
                access_token, new_refresh_token = refresh_access_token(
                    session, client_id, refresh_token, scope
                )

                if args.save_token and new_refresh_token:
                    lines[line_index] = "----".join(
                        [email_address, password, client_id, new_refresh_token]
                    )
                    try:
                        save_lines(path, lines)
                    except OSError:
                        # 保存失败仍可使用本次访问令牌读取邮件。
                        print("警告：新刷新令牌保存失败，请检查账号文件权限。")

                if args.protocol == "graph":
                    messages = fetch_via_graph(
                        session, access_token, args.limit, args.unread
                    )
                else:
                    messages = fetch_via_imap(
                        email_address, access_token, args.limit, args.unread
                    )
                print_messages(messages)

            except MailError as error:
                print(f"失败：{error}")
            except requests.RequestException:
                print("网络请求失败：请检查网络连接或稍后重试。")


if __name__ == "__main__":
    main()
