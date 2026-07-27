#!/usr/bin/env python3
"""QoderWork Global (qoder.com) — PAT → token → 领取积分 全链路测试。

流程:
  1. 从 accounts.csv 取一个 PAT
  2. POST openapi.qoder.sh/api/v1/jobToken/exchange → 拿 jt-/jrt-
  3. GET /api/v2/quota/usage → 查当前积分
  4. GET /sash/api/v1/me/pro-upgrade/eligibility → 查是否可领
  5. POST /sash/api/v1/me/pro-upgrade/claim → 领取
  6. GET /api/v2/quota/usage → 查领取后积分

用法: python3 /root/qoderwork/test_global_claim.py [--email EMAIL]
"""
import csv, json, sys, urllib.request, urllib.error

OPENAPI = "https://openapi.qoder.sh"


def http(method, url, body=None, token=None, timeout=20):
    h = {"Accept": "application/json", "User-Agent": "QoderWork"}
    if token:
        h["Authorization"] = f"Bearer {token}"
    data = None
    if body is not None:
        data = json.dumps(body).encode()
        h["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            raw = r.read().decode()
            try:
                return r.status, json.loads(raw)
            except Exception:
                return r.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, raw
    except Exception as e:
        return -1, str(e)


def load_pat(email=None):
    with open("/root/qoder-register/accounts.csv") as f:
        rows = [r for r in csv.DictReader(f) if r.get("pat", "").startswith("pt-")]
    if not rows:
        print("❌ no PATs in accounts.csv")
        sys.exit(1)
    if email:
        for r in rows:
            if r.get("email", "").lower() == email.lower():
                return r
        print(f"❌ email not found: {email}")
        sys.exit(1)
    return rows[0]


def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--email", default=None)
    args = ap.parse_args()

    acc = load_pat(args.email)
    pat = acc["pat"]
    email = acc["email"]
    print(f"[1] account: {email}")
    print(f"    PAT: {pat[:15]}...")

    # Step 2: PAT → jobToken
    st, tok = http("POST", f"{OPENAPI}/api/v1/jobToken/exchange", {"personal_token": pat})
    print(f"\n[2] jobToken/exchange: HTTP {st}")
    if st != 200 or not isinstance(tok, dict) or not tok.get("token"):
        print(f"    ❌ failed: {str(tok)[:300]}")
        sys.exit(1)
    jt = tok["token"]
    jrt = tok.get("refresh_token", "")
    print(f"    jt: {jt[:15]}...  jrt: {jrt[:15]}...")
    print(f"    expires_in: {tok.get('expires_in')}  expires_at: {tok.get('expires_at')}")

    # Step 3: 查当前积分
    st, quota = http("GET", f"{OPENAPI}/api/v2/quota/usage", token=jt)
    print(f"\n[3] quota/usage: HTTP {st}")
    if st == 200 and isinstance(quota, dict):
        uq = quota.get("userQuota", {})
        print(f"    total: {uq.get('total')}  used: {uq.get('used')}  remaining: {uq.get('remaining')}")
        print(f"    userType: {quota.get('userType')}  isQuotaExceeded: {quota.get('isQuotaExceeded')}")
    else:
        print(f"    body: {str(quota)[:300]}")

    # Step 4: pro-upgrade eligibility
    st, elig = http("GET", f"{OPENAPI}/sash/api/v1/me/pro-upgrade/eligibility", token=jt)
    print(f"\n[4] pro-upgrade/eligibility: HTTP {st}")
    print(f"    {str(elig)[:300]}")

    # Step 5: claim
    if st == 200 and isinstance(elig, dict) and elig.get("eligible"):
        st, claim = http("POST", f"{OPENAPI}/sash/api/v1/me/pro-upgrade/claim", {}, token=jt)
        print(f"\n[5] pro-upgrade/claim: HTTP {st}")
        print(f"    {str(claim)[:300]}")
    else:
        print(f"\n[5] skipped (not eligible)")

    # Step 6: 领取后积分
    st, quota2 = http("GET", f"{OPENAPI}/api/v2/quota/usage", token=jt)
    print(f"\n[6] quota/usage (after): HTTP {st}")
    if st == 200 and isinstance(quota2, dict):
        uq = quota2.get("userQuota", {})
        print(f"    total: {uq.get('total')}  used: {uq.get('used')}  remaining: {uq.get('remaining')}")
    else:
        print(f"    body: {str(quota2)[:300]}")

    # Step 7: userinfo
    st, ui = http("GET", f"{OPENAPI}/api/v1/userinfo", token=jt)
    print(f"\n[7] userinfo: HTTP {st}")
    if st == 200:
        print(f"    uid: {ui.get('id')}  name: {ui.get('name')}  user_type: {ui.get('user_type')}")


if __name__ == "__main__":
    main()
