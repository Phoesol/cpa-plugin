#!/usr/bin/env python3
"""QoderWork Global — 用 camoufox 走 device OAuth 流程，验证登录客户端是否触发 300 积分。

流程：
  1. 从 accounts.csv 取一个 PAT
  2. 用 PAT 获取已有 cookies（或直接用 camoufox 打开 qoder.com 登录）
  3. 生成 PKCE + device auth URL
  4. camoufox 打开 auth URL → 已有 cookie 则自动授权 → 点 Continue
  5. 轮询 deviceToken/poll → 拿到 dt-/drt-
  6. 查 quota/usage → 看 total 是否 300
  7. 查 pro-upgrade/eligibility → 看是否 eligible
  8. 如果 eligible → claim → 再查 quota

用法: python3 /root/qoderwork/test_global_oauth_claim.py [--email EMAIL]
"""
import argparse, base64, csv, hashlib, json, os, sys, time, urllib.parse, urllib.request, uuid

WEBSITE = "https://qoder.com"
OPENAPI = "https://openapi.qoder.sh"
CLIENT_ID = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
REDIRECT_URI = "qoder-work://"
AUTH_DIR = "/root/qoder-register/auth"


def http(method, url, body=None, token=None, timeout=20):
    h = {"Accept": "application/json", "User-Agent": "QoderWork"}
    if token: h["Authorization"] = f"Bearer {token}"
    data = json.dumps(body).encode() if body is not None else None
    if data: h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            try: return r.status, json.loads(raw)
            except: return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try: return e.code, json.loads(raw)
        except: return e.code, raw
    except Exception as e:
        return -1, str(e)


def make_pkce():
    verifier = base64.urlsafe_b64encode(os.urandom(48)).decode().rstrip("=")[:64]
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
    return verifier, challenge


def load_cookies(auth_file):
    """Load cookies from qoder-register auth JSON (playwright format)."""
    if not os.path.exists(auth_file):
        return None
    try:
        d = json.load(open(auth_file))
        cookies = d.get("cookies", [])
        # 过滤 qoder.com 域的
        qoder_cookies = [c for c in cookies if "qoder.com" in c.get("domain", "")]
        return qoder_cookies if qoder_cookies else None
    except:
        return None


def find_auth_file(email):
    """Find auth file by email."""
    email_slug = email.replace("@", "_at_").replace(".", "_")
    path = os.path.join(AUTH_DIR, f"auth_{email_slug}.json")
    return path if os.path.exists(path) else None


def run_oauth_with_camoufox(auth_url, cookies):
    """Use camoufox to complete device authorization."""
    from camoufox.sync_api import Camoufox
    
    with Camoufox(headless=True) as browser:
        ctx = browser.new_context()
        
        # Seed cookies if available
        if cookies:
            playwright_cookies = []
            for c in cookies:
                sc = c.get("sameSite", "")
                if sc not in ("Lax", "Strict", "None"):
                    sc = "Lax"
                playwright_cookies.append({
                    "name": c.get("name", ""),
                    "value": c.get("value", ""),
                    "domain": c.get("domain", ""),
                    "path": c.get("path", "/"),
                    "secure": c.get("secure", False),
                    "httpOnly": c.get("httpOnly", False),
                    "sameSite": sc,
                })
            ctx.add_cookies(playwright_cookies)
            print(f"[+] Seeded {len(playwright_cookies)} cookies")
        
        page = ctx.new_page()
        
        # 打开 auth URL
        print(f"[+] Opening auth URL...")
        page.goto(auth_url, wait_until="domcontentloaded", timeout=60000)
        time.sleep(3)
        
        # 尝试点授权按钮
        for round_i in range(6):
            url = page.url
            title = page.title()
            print(f"  round{round_i}: {url[:80]} title={title}")
            
            # 检查是否已成功
            try:
                txt = page.locator("body").inner_text(timeout=2000)
            except:
                txt = ""
            low = txt.lower()
            if any(k in low for k in ["authorized", "success", "已授权", "can close", "return to qoder"]):
                print("  ✓ looks like success")
                break
            
            # 点 Continue/Authorize/Allow
            clicked = False
            for sel in [
                "button:has-text('Continue')",
                "button:has-text('Authorize')",
                "button:has-text('Allow')",
                "button:has-text('Confirm')",
                "button:has-text('继续')",
                "button:has-text('授权')",
                "button:has-text('同意')",
            ]:
                try:
                    loc = page.locator(sel).first
                    if loc.count() and loc.is_visible():
                        loc.click(timeout=2000)
                        print(f"  clicked: {sel}")
                        clicked = True
                        time.sleep(2)
                        break
                except:
                    continue
            
            if not clicked:
                time.sleep(2)
        
        ctx.close()
    return True


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--email", default=None, help="Specific email from accounts.csv")
    args = ap.parse_args()
    
    # 1. 取 PAT
    with open("/root/qoder-register/accounts.csv") as f:
        rows = [r for r in csv.DictReader(f) if r.get("pat", "").startswith("pt-")]
    if not rows:
        print("❌ no PATs")
        sys.exit(1)
    
    if args.email:
        acc = next((r for r in rows if r["email"].lower() == args.email.lower()), None)
        if not acc:
            print(f"❌ email not found: {args.email}")
            sys.exit(1)
    else:
        acc = rows[0]
    
    email = acc["email"]
    pat = acc["pat"]
    print(f"[1] account: {email}")
    
    # 2. 查 auth file 有没有 cookies
    auth_file = find_auth_file(email)
    cookies = load_cookies(auth_file) if auth_file else None
    if cookies:
        print(f"[2] found {len(cookies)} qoder.com cookies in {auth_file}")
    else:
        print("[2] no cookies found, will try without")
    
    # 3. 先查 PAT 换 jt- 后的当前 quota（对比用）
    st, tok = http("POST", f"{OPENAPI}/api/v1/jobToken/exchange", {"personal_token": pat})
    if st == 200 and isinstance(tok, dict) and tok.get("token"):
        jt = tok["token"]
        st2, q_before = http("GET", f"{OPENAPI}/api/v2/quota/usage", token=jt)
        if st2 == 200:
            uq = q_before.get("userQuota", {})
            print(f"[3] PAT quota (before OAuth): total={uq.get('total')} remaining={uq.get('remaining')}")
    
    # 4. 生成 device auth URL
    verifier, challenge = make_pkce()
    nonce = str(uuid.uuid4())
    machine_id = str(uuid.uuid4())
    q = urllib.parse.urlencode({
        "challenge": challenge, "challenge_method": "S256", "nonce": nonce,
        "machine_id": machine_id, "client_id": CLIENT_ID, "redirect_uri": REDIRECT_URI,
    })
    auth_url = f"{WEBSITE}/device/selectAccounts?{q}"
    poll_url = f"{OPENAPI}/api/v1/deviceToken/poll?{urllib.parse.urlencode({'nonce': nonce, 'verifier': verifier, 'challenge_method': 'S256'})}"
    print(f"[4] auth URL generated")
    
    # 5. camoufox 授权
    print("[5] starting camoufox...")
    run_oauth_with_camoufox(auth_url, cookies)
    
    # 6. 轮询拿 dt-
    print("[6] polling for device token...")
    deadline = time.time() + 300
    dt = None
    drt = None
    while time.time() < deadline:
        st, body = http("GET", poll_url)
        if st == 200 and isinstance(body, dict) and (body.get("token") or body.get("device_token")):
            dt = body.get("token") or body.get("device_token")
            drt = body.get("refresh_token")
            print(f"  ✅ got token: dt={dt[:15]}... drt={drt[:15]}...")
            break
        time.sleep(3)
    
    if not dt:
        print("❌ polling timeout — authorization may not have completed")
        sys.exit(1)
    
    # 7. 查 quota（用 dt-）
    st, q_after = http("GET", f"{OPENAPI}/api/v2/quota/usage", token=dt)
    if st == 200:
        uq = q_after.get("userQuota", {})
        print(f"[7] quota (after OAuth): total={uq.get('total')} remaining={uq.get('remaining')}")
        if uq.get("total", 0) > 0:
            print(f"  🎉 有积分！total={uq.get('total')}")
        else:
            print("  ⚠️ 仍然 0 积分")
    else:
        print(f"[7] quota error: {st} {q_after}")
    
    # 8. 查 plan
    st, plan = http("GET", f"{OPENAPI}/api/v2/user/plan", token=dt)
    if st == 200:
        print(f"[8] plan: {plan.get('plan_tier_name')} user_type={plan.get('user_type')}")
    
    # 9. 查 pro-upgrade
    st, elig = http("GET", f"{OPENAPI}/sash/api/v1/me/pro-upgrade/eligibility", token=dt)
    print(f"[9] pro-upgrade eligibility: {st} → {elig}")
    if isinstance(elig, dict) and elig.get("eligible"):
        st, claim = http("POST", f"{OPENAPI}/sash/api/v1/me/pro-upgrade/claim", token=dt, body={})
        print(f"[10] pro-upgrade claim: {st} → {claim}")
        st, q_final = http("GET", f"{OPENAPI}/api/v2/quota/usage", token=dt)
        if st == 200:
            uq = q_final.get("userQuota", {})
            print(f"[11] quota (after claim): total={uq.get('total')} remaining={uq.get('remaining')}")
    
    # 12. 保存结果
    result = {"email": email, "dt": dt, "drt": drt, "quota_before": q_before if 'q_before' in dir() else None, 
              "quota_after": q_after if 'q_after' in dir() else None}
    with open("/root/qoderwork/global_oauth_result.json", "w") as f:
        json.dump(result, f, indent=2, ensure_ascii=False)
    print(f"\n[✓] saved to /root/qoderwork/global_oauth_result.json")


if __name__ == "__main__":
    main()
