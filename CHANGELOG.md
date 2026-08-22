# Changelog

## [0.5.2](https://github.com/coderage-labs/spillway/compare/v0.5.1...v0.5.2) (2026-08-22)


### Fixes

* keep the systemd unit called spillway.service ([05f5981](https://github.com/coderage-labs/spillway/commit/05f5981b76aa9e7bc549792e6bb2de7e965b3459))
* three dashboard defects found by looking at it ([2b389f9](https://github.com/coderage-labs/spillway/commit/2b389f9df07016123f0ab2617de23e6ed89f414a))


### Internal

* exercise the service install for real, on each platform ([af04ed5](https://github.com/coderage-labs/spillway/commit/af04ed50cb94dd05370cde5a7649f1a0e86d2dbb))

## [0.5.1](https://github.com/coderage-labs/spillway/compare/v0.5.0...v0.5.1) (2026-08-22)


### Fixes

* an upgrade on Windows left the machine with no daemon at all ([9ec1b2f](https://github.com/coderage-labs/spillway/commit/9ec1b2f028043db727c8911fd0644dbd04352603))
* retry the Windows task start until the daemon actually stays up ([6feb497](https://github.com/coderage-labs/spillway/commit/6feb497e38b4781bf1f972de7399fc780cb5eb2e))
* the Windows task ran a shell, so stopping it orphaned the daemon ([63af7c0](https://github.com/coderage-labs/spillway/commit/63af7c02c4ac4fb61082e072f94a888a888f8ba9))
* the Windows task XML was rejected by every real scheduler ([aaa3ad5](https://github.com/coderage-labs/spillway/commit/aaa3ad562637865325293f21a837aa47209d776d))


### Internal

* a manually-run Windows probe ([e74ad6f](https://github.com/coderage-labs/spillway/commit/e74ad6fbaa28c0f52e3ea7ada670b549b91c41cd))
* feed the task XML to a real Task Scheduler on CI ([5846bdd](https://github.com/coderage-labs/spillway/commit/5846bdd3caa6d322e13836397b45c80908b93eb2))
* let the probe tell a slow stop from a leaked daemon ([27a4136](https://github.com/coderage-labs/spillway/commit/27a413675761f712935ccb79582993c78b415383))
* make the Windows probe explain /End before judging it ([580a15b](https://github.com/coderage-labs/spillway/commit/580a15bb23c8d4385e2bcc37f7f20f27bf4d6689))
* measure the gap instead of sampling it once ([24b2810](https://github.com/coderage-labs/spillway/commit/24b2810713b199661b90f1b2e74525f208684fdb))
* PLANTED invalid task Version - verifying CI actually runs this ([7c4f7a7](https://github.com/coderage-labs/spillway/commit/7c4f7a7fcda19aabdcafb0067ea0b16179f5e6a6))
* probe the scoop shim, and stop the reinstall step testing nothing ([b6f3d6c](https://github.com/coderage-labs/spillway/commit/b6f3d6c58f2a6d99cf55749fb2630d1e9c984e9f))
* the shim step passed without asking the question ([17c327c](https://github.com/coderage-labs/spillway/commit/17c327c5bf2fd19340ac0fd6b30eaedd1488a731))
* write the task XML the way serviceInstall writes it ([8fc2b3a](https://github.com/coderage-labs/spillway/commit/8fc2b3a4a84ff833b73830be63058802117a71d7))

## [0.5.0](https://github.com/coderage-labs/spillway/compare/v0.4.0...v0.5.0) (2026-08-22)


### Features

* keep secrets in a 0600 file where there is no keychain ([e6938cf](https://github.com/coderage-labs/spillway/commit/e6938cf4a3f0d7c0391a82334899e8e571e562d5))
* run as a systemd user unit on Linux ([aca5c39](https://github.com/coderage-labs/spillway/commit/aca5c3932ee53bba1ab2e0a4e87ab8f680ccf5c9))


### Fixes

* don't assert cross-process file locking on windows ([0c665d9](https://github.com/coderage-labs/spillway/commit/0c665d9b845eff0c60be026747db2444cd9c748d))
* restart the service on upgrade, and zap it on uninstall ([0463f3c](https://github.com/coderage-labs/spillway/commit/0463f3c818ad3aa3b7a078be287e78ce7af1bac6))
* Windows reinstall left the old daemon running, and add a scoop bucket ([1d7ed8d](https://github.com/coderage-labs/spillway/commit/1d7ed8de0c182586ddb82b956a1459bc4a132d73))

## [0.4.0](https://github.com/coderage-labs/spillway/compare/v0.3.0...v0.4.0) (2026-08-22)


### Features

* `spillway accounts priority`, and show priority in the listing ([9593693](https://github.com/coderage-labs/spillway/commit/9593693fc706ad26e4cb6729ac6b6b5657b83c7b))


### Fixes

* Kimi's concurrency cap is not a quota window ([31dc8e2](https://github.com/coderage-labs/spillway/commit/31dc8e28032e1d6182f2c133f0a90784fabf871e))
* one vocabulary for quota window names across providers ([cfd8e01](https://github.com/coderage-labs/spillway/commit/cfd8e01e4ba0126c468977ce7b5d7a5ee42a6a54))
* retry launchd bootstrap whatever the reason it failed ([46177d6](https://github.com/coderage-labs/spillway/commit/46177d66eb1bbb258182421267e547ea039f9e8d))
* the plugin called an account billed when it was not ([ce174b3](https://github.com/coderage-labs/spillway/commit/ce174b3e2219719f61ed11fe0b5cfe625900f618))

## [0.3.0](https://github.com/coderage-labs/spillway/compare/v0.2.0...v0.3.0) (2026-08-22)


### Features

* `spillway install` sets up everything in one command ([e4fd537](https://github.com/coderage-labs/spillway/commit/e4fd537a9d1b04c356999cafb8988eb93168e6f1))
* `spillway status --json`, and one way to reach the admin API ([d433490](https://github.com/coderage-labs/spillway/commit/d433490e66e320a982ae59efbf77f6d5b81e8e2c))
* a Claude Code plugin that reports pool status in-session ([9aec2f5](https://github.com/coderage-labs/spillway/commit/9aec2f544120751a2be7ed1f299f96ec4c8339f5))


### Fixes

* name the plugin command `status`, not `spillway` ([bae4d2c](https://github.com/coderage-labs/spillway/commit/bae4d2cf3f754d2d7bdc3f6073840264623c594a))
* stop kimi quota windows accumulating on every poll ([4f6bcd5](https://github.com/coderage-labs/spillway/commit/4f6bcd526a6ba690881ed91d4a6828f15d7c6be7))


### Internal

* drop the /spawn plugin to v2 ([48433d2](https://github.com/coderage-labs/spillway/commit/48433d29ace5229fcaf47133c0e665cba1409613))

## [0.2.0](https://github.com/coderage-labs/spillway/compare/v0.1.2...v0.2.0) (2026-08-22)


### ⚠ BREAKING CHANGES

* remove `spillway hook` — it cannot do what it promised

### Features

* default model map for Kimi, measured from its own /v1/models ([51d6ccd](https://github.com/coderage-labs/spillway/commit/51d6ccd01fe1d5f8000d3071bfbfd8cdef8b3e01))


### Fixes

* negotiate ALPN tolerantly in MITM mode instead of refusing the connection ([596ec3a](https://github.com/coderage-labs/spillway/commit/596ec3a70e18d0daeb2886afb0d353c86aaa88e5))
* negotiate ALPN tolerantly in MITM mode, and log CONNECT ([1c8e127](https://github.com/coderage-labs/spillway/commit/1c8e127d5e918a808aa50b031a3c3b2d1b9ce7ad))


### Internal

* one file per provider, and no provider named outside the registry ([b2a2c3f](https://github.com/coderage-labs/spillway/commit/b2a2c3f0c9aff1df32331c6da123caedaa7b8eef))
* remove `spillway hook` — it cannot do what it promised ([4324b3b](https://github.com/coderage-labs/spillway/commit/4324b3b22c8a4f49db73c78721cc0b148e8b28b5))


### Documentation

* Remote Control is fixed — correcting the previous commit ([4cff7e2](https://github.com/coderage-labs/spillway/commit/4cff7e26cc2162d3349e19b70b101962352fa512))
* stop explaining a feature that no longer exists ([2af2673](https://github.com/coderage-labs/spillway/commit/2af26736a6b4ef097e151a609a8bce927efee456))

## [0.1.2](https://github.com/coderage-labs/spillway/compare/v0.1.1...v0.1.2) (2026-08-22)


### Fixes

* record the invoked path, not the symlink target ([2a45f15](https://github.com/coderage-labs/spillway/commit/2a45f155c7c2d94f3ded05d2aa6373ec3dd0169c))


### Internal

* give the selfPath fixtures a Windows-executable extension ([944e8bb](https://github.com/coderage-labs/spillway/commit/944e8bbcbe2b8de8918ba1c7446eb175eecd651e))

## [0.1.1](https://github.com/coderage-labs/spillway/compare/v0.1.0...v0.1.1) (2026-08-22)


### Internal

* build releases with goreleaser and publish a Homebrew cask ([e973fbc](https://github.com/coderage-labs/spillway/commit/e973fbcb22b7cb9f6f802c3d8f6cd412290b2596))

## 0.1.0 (2026-08-22)


### Features

* account labels; fix wave loop seam ([6312267](https://github.com/coderage-labs/spillway/commit/6312267532e58ae6d04c531eeb9c8060926354d9))
* account pool with sticky rotation and 429 failover ([68d689c](https://github.com/coderage-labs/spillway/commit/68d689cf5e7a5ad553a6a485bcda2a444e9e9490))
* account priority ([3b942ef](https://github.com/coderage-labs/spillway/commit/3b942ef8315fecd8cc3eec882ad4671f666477ae))
* account_uuid rewrite on injection ([f727404](https://github.com/coderage-labs/spillway/commit/f727404601e8c507b4cc0c35a874a5b94256d3d8))
* admin listener can bind a unix socket (§5) ([b7bf7f0](https://github.com/coderage-labs/spillway/commit/b7bf7f0f13f210d31ac93baaff7f3243b8bb314c))
* capability preflight and provider-pinned rotation ([7421b15](https://github.com/coderage-labs/spillway/commit/7421b153da71d1709649204e0bab47c5652afb17))
* Claude OAuth credential import + auth injection ([47ab1f3](https://github.com/coderage-labs/spillway/commit/47ab1f334fdd990fc8d9f44f9c3a13db7b366983))
* dashboard redesign — liquid tanks, headroom history, burn-rate projection ([efc1470](https://github.com/coderage-labs/spillway/commit/efc147034c3e4e67820c564729c29eb25bf70eef))
* drain all free quota before billing; stop a refused overage account spinning ([adbdcbf](https://github.com/coderage-labs/spillway/commit/adbdcbf58933df91c55f3d6e83f8cf9dbf1001d3))
* edit settings from the dashboard ([16d90e2](https://github.com/coderage-labs/spillway/commit/16d90e2477f693d0d9b196fb05a93ea7d41a1674))
* egress interface, modelMap globs, configurable buffer cap, real notifications ([eb45b9f](https://github.com/coderage-labs/spillway/commit/eb45b9fb342dc5b027ed3ebba5c74f3d939a15d4))
* handle extra usage as a last-resort paid tier; stop probes billing for it ([60ae732](https://github.com/coderage-labs/spillway/commit/60ae73237e56afef00a3bc061ca7cb708aba94d2))
* hold-until-reset on pool exhaustion ([a0c0752](https://github.com/coderage-labs/spillway/commit/a0c0752c6fe47ea9d0d5403cc7ca614155093815))
* import recognises the claude CLI's own account and refuses to copy it ([a1532d0](https://github.com/coderage-labs/spillway/commit/a1532d00ae2cf6c1ca5c667a7dc702008cc1b2e3))
* kimi provider — device-flow login, modelMap, /usages quota polling ([aa18618](https://github.com/coderage-labs/spillway/commit/aa186185d51c65322544147c496cc0bbd48773cd))
* layered wave tanks, drop monospace UI ([170069c](https://github.com/coderage-labs/spillway/commit/170069ca6674f8080e012dc4f4a6b62770521754))
* MITM CONNECT mode with keychain CA + identity pass-throughs ([bbf85b0](https://github.com/coderage-labs/spillway/commit/bbf85b0587eb073e26181db112b40a2cbbcf8b88))
* move account secrets to OS keychain ([8269d6b](https://github.com/coderage-labs/spillway/commit/8269d6ba975a7c898172647ec0470735ebe5c44e))
* no admin token on loopback; require it off-loopback ([b5d034d](https://github.com/coderage-labs/spillway/commit/b5d034da7c692361d3bbdf1117edff2c53d0cf1a))
* OAuth token refresh with singleflight + source-aware persistence ([c18922d](https://github.com/coderage-labs/spillway/commit/c18922ddad674b0ca8867707191890bcde90199f))
* own swell path, desynchronised tanks ([186a051](https://github.com/coderage-labs/spillway/commit/186a051b79f46566685384d94a1c52c0b4d45f49))
* packaging — settings.json hook, launchd service, /spawn plugin ([74b8920](https://github.com/coderage-labs/spillway/commit/74b8920279fac509656f43472732049277d33a63))
* predictive rotation from quota signals ([0c62056](https://github.com/coderage-labs/spillway/commit/0c620566524a2aa3d4d575acc47899e287b209dd))
* prove predictive rotation on a real spent account; stop calling it healthy ([d17d133](https://github.com/coderage-labs/spillway/commit/d17d13373220d568b74b9f4a8cf2d08e4018f5a5))
* replay suite, live canary, client-version matrix (§6.8) ([85d42bf](https://github.com/coderage-labs/spillway/commit/85d42bfb7acef20e79887e2a412e9c5490ba3d97))
* request log, admin API, embedded web UI ([66ddc14](https://github.com/coderage-labs/spillway/commit/66ddc14e7790820ad9a61570f5fe671e22abb57b))
* spillway login claude + accounts commands ([ef8a2e6](https://github.com/coderage-labs/spillway/commit/ef8a2e66fe54315f1bf2ebefadcae3029fc74db6))
* startup quota probe; fix wave drift mechanism ([7a86285](https://github.com/coderage-labs/spillway/commit/7a86285df0f76b77aaf17eac30bb5706ab66180f))
* statusline command; record the model actually served; fix idle probes ([630b1e1](https://github.com/coderage-labs/spillway/commit/630b1e1aafe0621138eb30ec83dc5bd6336a4307))
* statusline install/uninstall; scheduled credential refresh; rewrite README ([a294618](https://github.com/coderage-labs/spillway/commit/a2946189be08bd87830a38e64e7a47f3bbccb39d))
* surface holds, degraded accounts and reset countdowns ([182c2c1](https://github.com/coderage-labs/spillway/commit/182c2c1dbc96492806bdd9c23e286a6e8ae114f1))
* surface quota tiers honestly; document egress, socket and the rest ([8301bce](https://github.com/coderage-labs/spillway/commit/8301bce2e092a09b44ddd3354ed636bac8925bb6))
* walking skeleton — streaming reverse proxy scaffold ([01af146](https://github.com/coderage-labs/spillway/commit/01af14638bb9a42c272792ce75b5e6c1104fd874))


### Fixes

* `accounts` read the live keychain expiry, not a stale yaml snapshot ([33251a8](https://github.com/coderage-labs/spillway/commit/33251a8e541bbd1f5e064df15464349749db668d))
* dashboard froze during normal traffic — poll accounts/requests, add SSE heartbeat ([a7604cc](https://github.com/coderage-labs/spillway/commit/a7604cc8b713746fc50bc977577cdc0b0c732973))
* deflake hold test — retry-after reset hint, wall-second truncation made it racy ([6acfdc8](https://github.com/coderage-labs/spillway/commit/6acfdc8a39c9b6f1e1ac94128c770ec1e04dbb4e))
* drop X-Forwarded-For injection — violates §4 mutation budget, signals proxying to upstream ([2f884f0](https://github.com/coderage-labs/spillway/commit/2f884f09884559a39277e87a428226df48e383a8))
* kimi 401 body classification, /usages string parsing, model id docs ([108414d](https://github.com/coderage-labs/spillway/commit/108414d1ec5ea4db34b72decdba1005fabb34a2c))
* label the account settings controls; record the missing provider plugin ([0d91721](https://github.com/coderage-labs/spillway/commit/0d917218cdc18dda5e5d3de3102dc046ed92d0cf))
* no colour join between swell and water body ([9a9486c](https://github.com/coderage-labs/spillway/commit/9a9486c99f1b79095de723e291a4bd3ac6766db7))
* one swell per tank, not a train of ripples ([ad10f70](https://github.com/coderage-labs/spillway/commit/ad10f706b729cbacb76fb55b77360bcd20daa9fe))
* RC server-mode pass-throughs + sanitize inherited env in run ([45cca2f](https://github.com/coderage-labs/spillway/commit/45cca2f739ca983822a5cea0147cbe9858d3125c))
* read-only admin endpoints return 405 instead of answering non-GET ([c1fcfea](https://github.com/coderage-labs/spillway/commit/c1fcfeaac15ee6cc8b6dc11231d4be147334029b))
* static-key accounts must not be refresh-disabled ([67ff14e](https://github.com/coderage-labs/spillway/commit/67ff14e343737ce63825df6cd9bd56969283c8e3))
* stop rebuilding tanks on every poll — it restarted every wave ([10a6898](https://github.com/coderage-labs/spillway/commit/10a68981aaf8326c6c1cf018954de41f80378fa8))
* wave phases were discarded, so every tank started in step ([d17ee65](https://github.com/coderage-labs/spillway/commit/d17ee65af5ba2b5b0af0bff6effeb447a35ee4d3))


### Internal

* adopt release-please for versioning and changelogs ([fc68ab8](https://github.com/coderage-labs/spillway/commit/fc68ab830a3d7d437112cd698f5a817114e6571a))
* build, vet, gofmt, race tests and govulncheck on push and PR ([bff56b1](https://github.com/coderage-labs/spillway/commit/bff56b146832fb6b2845f1bd6128522eb91e3536))
* dashboard JS smoke test (renders + poll tick refetches) ([da5f0a3](https://github.com/coderage-labs/spillway/commit/da5f0a3d64baea0c7a3a28f2e3d1debbbf274fac))
* extract the Provider registry the design specified in §3 ([1fe4509](https://github.com/coderage-labs/spillway/commit/1fe45093af4b3aa768ec34189ac3c7a652cd4c82))
* start at 0.1.0, and stop the release PR needing manual CI approval ([693ab05](https://github.com/coderage-labs/spillway/commit/693ab05041546ee3bc7229f6ae7676b163c17135))
* stop asserting the request log before the write happens ([e0987e5](https://github.com/coderage-labs/spillway/commit/e0987e5a6ec6c4347dff13cfbee1e089b5d8cc33))
* tag-driven release pipeline and a binary that says what it is ([8c56af7](https://github.com/coderage-labs/spillway/commit/8c56af78ffea676244346cacc0130c08ccbba4f1))


### Documentation

* settings, capability routing and cross-provider default in the README ([875e7c3](https://github.com/coderage-labs/spillway/commit/875e7c3f80243612d0e0fbcd96e0ffcbe900bb9d))
