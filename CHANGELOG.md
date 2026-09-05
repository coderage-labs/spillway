# Changelog

## [0.19.4](https://github.com/coderage-labs/spillway/compare/v0.19.3...v0.19.4) (2026-09-05)


### Fixes

* give session_hash a key that identifies a session, not a client ([#141](https://github.com/coderage-labs/spillway/issues/141)) ([#160](https://github.com/coderage-labs/spillway/issues/160)) ([193e653](https://github.com/coderage-labs/spillway/commit/193e653ef4ae30c8b70325c8ee1e4939311512a7))
* re-test a stale provider overage refusal instead of believing it forever ([#151](https://github.com/coderage-labs/spillway/issues/151)) ([#156](https://github.com/coderage-labs/spillway/issues/156)) ([2a70ddc](https://github.com/coderage-labs/spillway/commit/2a70ddc0b936eb447e15f1f0e78e684d4699fb62))

## [0.19.3](https://github.com/coderage-labs/spillway/compare/v0.19.2...v0.19.3) (2026-09-04)


### Fixes

* probe a spent window that costs nothing to re-measure ([#152](https://github.com/coderage-labs/spillway/issues/152)) ([#153](https://github.com/coderage-labs/spillway/issues/153)) ([23bf379](https://github.com/coderage-labs/spillway/commit/23bf379aafc977c11cffb92d28e3641531ec5243))
* wake a family-rejected hold on its own window deadline ([#140](https://github.com/coderage-labs/spillway/issues/140)) ([#155](https://github.com/coderage-labs/spillway/issues/155)) ([9a40599](https://github.com/coderage-labs/spillway/commit/9a40599e212aba5d6afd47d68e52f303acb34581))

## [0.19.2](https://github.com/coderage-labs/spillway/compare/v0.19.1...v0.19.2) (2026-09-02)


### Fixes

* record a window's real measurement time, not the sampler tick ([#138](https://github.com/coderage-labs/spillway/issues/138)) ([#149](https://github.com/coderage-labs/spillway/issues/149)) ([4967d4f](https://github.com/coderage-labs/spillway/commit/4967d4fe027f1216526382f5a53c5464267b4562))
* refuse a pin the provider would bill, not just one spillway would ([#139](https://github.com/coderage-labs/spillway/issues/139)) ([#147](https://github.com/coderage-labs/spillway/issues/147)) ([dfc238b](https://github.com/coderage-labs/spillway/commit/dfc238be1893961874af70a1ba0a2bc8adf9a626))

## [0.19.1](https://github.com/coderage-labs/spillway/compare/v0.19.0...v0.19.1) (2026-09-02)


### Fixes

* clamp negative headroom in spillway status ([#112](https://github.com/coderage-labs/spillway/issues/112)) ([#145](https://github.com/coderage-labs/spillway/issues/145)) ([1910bf0](https://github.com/coderage-labs/spillway/commit/1910bf06523dcd6c97b6bd369581f7d74379d3e5))
* expire a spent quota window at its own reset instead of trusting it forever ([#137](https://github.com/coderage-labs/spillway/issues/137)) ([709d77e](https://github.com/coderage-labs/spillway/commit/709d77ecc12302df273778cb5dd18e407cd1eee0))
* stop a cancelled client request rotating through the account pool ([#142](https://github.com/coderage-labs/spillway/issues/142)) ([4108454](https://github.com/coderage-labs/spillway/commit/4108454d390bd8004f225ab71e6c690b60155cda))

## [0.19.0](https://github.com/coderage-labs/spillway/compare/v0.18.0...v0.19.0) (2026-09-02)


### Features

* apply config changed outside the CLI without a restart ([#84](https://github.com/coderage-labs/spillway/issues/84)) ([9430731](https://github.com/coderage-labs/spillway/commit/9430731648d748f21ec667814f049fad1de9ab9e))
* measure request-prefix instability against real cache spend (refs [#111](https://github.com/coderage-labs/spillway/issues/111)) ([361c755](https://github.com/coderage-labs/spillway/commit/361c7559a6487f96a324b99c4181fca7afb67699))


### Fixes

* decode Brotli responses so the usage sniffer stops recording zeros ([#126](https://github.com/coderage-labs/spillway/issues/126)) ([acc4702](https://github.com/coderage-labs/spillway/commit/acc47020fcfb11e2be583a57d2f22357db0ebbf0))
* remove a data race in the loopback callback server and deflake its test ([#98](https://github.com/coderage-labs/spillway/issues/98)) ([fa3e7d8](https://github.com/coderage-labs/spillway/commit/fa3e7d8c8cf811fd297aab49a74baf53db0321a3))


### Internal

* make the byte-faithfulness guard actually compare bytes ([#128](https://github.com/coderage-labs/spillway/issues/128)) ([c197138](https://github.com/coderage-labs/spillway/commit/c197138cc7d3b1bc08fe3137bc8b327ee2a5956e))

## [0.18.0](https://github.com/coderage-labs/spillway/compare/v0.17.0...v0.18.0) (2026-09-02)


### Features

* notification channels — ntfy, webhook and Pushover destinations ([#101](https://github.com/coderage-labs/spillway/issues/101)) ([09960a1](https://github.com/coderage-labs/spillway/commit/09960a121845079c7ba8f3b50d462ef10f9e0675))

## [0.17.0](https://github.com/coderage-labs/spillway/compare/v0.16.0...v0.17.0) (2026-09-01)


### Features

* opt-in strip of the credit signals Claude Code's model gate latches on ([#118](https://github.com/coderage-labs/spillway/issues/118)) ([6624510](https://github.com/coderage-labs/spillway/commit/6624510abf78439f915b2c20f19dad647182f0db)), closes [#103](https://github.com/coderage-labs/spillway/issues/103)


### Fixes

* decode gzipped responses so the usage sniffer stops recording zeros ([#121](https://github.com/coderage-labs/spillway/issues/121)) ([04db493](https://github.com/coderage-labs/spillway/commit/04db493a43e4d7c7c31b5cc04e99ca6e40bc77f0))
* judge probe cost by the families the probe itself draws on ([#117](https://github.com/coderage-labs/spillway/issues/117)) ([6563096](https://github.com/coderage-labs/spillway/commit/65630968c8ae9c91e02160680200a6cd5185b714)), closes [#116](https://github.com/coderage-labs/spillway/issues/116)

## [0.16.0](https://github.com/coderage-labs/spillway/compare/v0.15.0...v0.16.0) (2026-09-01)


### Features

* record cache token usage so quota burn becomes explicable ([#110](https://github.com/coderage-labs/spillway/issues/110)) ([f15d5e3](https://github.com/coderage-labs/spillway/commit/f15d5e3f2cc31d5acd7c53d17dd6c369cbf7c5e5))


### Fixes

* merge quota windows by name so a silent response stops deleting them ([#100](https://github.com/coderage-labs/spillway/issues/100)) ([1faabf3](https://github.com/coderage-labs/spillway/commit/1faabf329750bc3805f5ec18d3e2847516f00e3b))

## [0.15.0](https://github.com/coderage-labs/spillway/compare/v0.14.1...v0.15.0) (2026-09-01)


### Features

* wake a held request when capacity appears, not just when the clock says so ([#105](https://github.com/coderage-labs/spillway/issues/105)) ([bf404fe](https://github.com/coderage-labs/spillway/commit/bf404fe0259e2e4db8e0356c504967525ad7b624))


### Fixes

* classify handshake failures by kind, not by platform error text ([#96](https://github.com/coderage-labs/spillway/issues/96)) ([9d58fb2](https://github.com/coderage-labs/spillway/commit/9d58fb21ec20816bdf5adae02301e83221d44c7a))
* order quota windows by length, then variant ([#106](https://github.com/coderage-labs/spillway/issues/106)) ([#109](https://github.com/coderage-labs/spillway/issues/109)) ([f5ba0c1](https://github.com/coderage-labs/spillway/commit/f5ba0c185663fb7e11e3616b1514e42a1057ad7a))
* stop an unindexed quota_samples query blocking startup ([#104](https://github.com/coderage-labs/spillway/issues/104)) ([a341d9c](https://github.com/coderage-labs/spillway/commit/a341d9cb9dfb25fc3905f41d746a30b5a6410fc8))

## [0.14.1](https://github.com/coderage-labs/spillway/compare/v0.14.0...v0.14.1) (2026-08-27)


### Fixes

* bench an account only to its soonest rejected window, and re-probe it ([#90](https://github.com/coderage-labs/spillway/issues/90)) ([e3d872a](https://github.com/coderage-labs/spillway/commit/e3d872ac5a6bf632b9b58ebcc1c171943df2b798))
* keep non-inference requests out of the pool and the hold path ([#91](https://github.com/coderage-labs/spillway/issues/91)) ([678e1aa](https://github.com/coderage-labs/spillway/commit/678e1aa01630d071e1fef31bc6c11773dc1a683a))

## [0.14.0](https://github.com/coderage-labs/spillway/compare/v0.13.0...v0.14.0) (2026-08-27)


### Features

* apply account adds and re-auth live to the running pool ([#87](https://github.com/coderage-labs/spillway/issues/87)) ([9e16f2a](https://github.com/coderage-labs/spillway/commit/9e16f2a21d8c8beab170f2254d00af702a4f2ecd))

## [0.13.0](https://github.com/coderage-labs/spillway/compare/v0.12.2...v0.13.0) (2026-08-27)


### Features

* warn a running session when a chain regeneration has stranded it ([#66](https://github.com/coderage-labs/spillway/issues/66)) ([429d0b1](https://github.com/coderage-labs/spillway/commit/429d0b1de68d7b1ad0bc14e3773aaef7db5cccff))


### Fixes

* apply account remove/priority/overage live to the running pool ([#83](https://github.com/coderage-labs/spillway/issues/83)) ([530e93a](https://github.com/coderage-labs/spillway/commit/530e93aa8ede649be85603ac938042b1848bd7e3))
* deprecate borrowed keychain credentials for pooling ([#81](https://github.com/coderage-labs/spillway/issues/81)) ([ad0d707](https://github.com/coderage-labs/spillway/commit/ad0d707597fbf68765da3e366f5d7091cb8e22a1))
* keep headroom chart labels inside the plot ([#75](https://github.com/coderage-labs/spillway/issues/75)) ([11d57e5](https://github.com/coderage-labs/spillway/commit/11d57e5d3449065eb99f437566d955eb69f527da))


### Documentation

* drop the hero caption ([8ff62bb](https://github.com/coderage-labs/spillway/commit/8ff62bb7b8e130d42f2e1b82ca1a209167f12d23)), closes [#72](https://github.com/coderage-labs/spillway/issues/72)
* show the dashboard in the README, over an enriched demo pool ([#72](https://github.com/coderage-labs/spillway/issues/72)) ([593393a](https://github.com/coderage-labs/spillway/commit/593393a7bf1f5af487a16da1e2d306e55dea3ee7))

## [0.12.2](https://github.com/coderage-labs/spillway/compare/v0.12.1...v0.12.2) (2026-08-24)


### Fixes

* give non-Node subprocesses a CA bundle that extends the system roots ([#64](https://github.com/coderage-labs/spillway/issues/64)) ([3e6398a](https://github.com/coderage-labs/spillway/commit/3e6398a74aaf329e591b5cb3b8a2410f1df33130))

## [0.12.1](https://github.com/coderage-labs/spillway/compare/v0.12.0...v0.12.1) (2026-08-24)


### Fixes

* don't regenerate the MITM CA on an ambiguous keychain error ([#65](https://github.com/coderage-labs/spillway/issues/65)) ([03fe65d](https://github.com/coderage-labs/spillway/commit/03fe65d531468bb04f7ddb21f73a3acb59e03162))
* mint every leaf up front and discard the CA private key ([#69](https://github.com/coderage-labs/spillway/issues/69)) ([5600a5f](https://github.com/coderage-labs/spillway/commit/5600a5f0ad6c443de974e778bf749ac6a2e87302))

## [0.12.0](https://github.com/coderage-labs/spillway/compare/v0.11.0...v0.12.0) (2026-08-23)


### Features

* rewrite advisor models nested in tools[] on cross-provider rotation ([#29](https://github.com/coderage-labs/spillway/issues/29)) ([ddb919b](https://github.com/coderage-labs/spillway/commit/ddb919bbdf564f4aa3244a75302c0df3b4ee88d1))

## [0.11.0](https://github.com/coderage-labs/spillway/compare/v0.10.0...v0.11.0) (2026-08-23)


### Features

* log when Representative-Claim disagrees with the static window guess ([#53](https://github.com/coderage-labs/spillway/issues/53)) ([d4bc0c8](https://github.com/coderage-labs/spillway/commit/d4bc0c8a2113b00d2b0c269200fd48f58fb93293))


### Fixes

* fail fast on a hold whose reset is beyond holdMax ([#55](https://github.com/coderage-labs/spillway/issues/55)) ([9fca136](https://github.com/coderage-labs/spillway/commit/9fca1364b65a2ad08a5beab0c54a947907fbc0ec))
* scope quota-429 exhaustion to the window that actually rejected ([#25](https://github.com/coderage-labs/spillway/issues/25), [#54](https://github.com/coderage-labs/spillway/issues/54)) ([8e7efbc](https://github.com/coderage-labs/spillway/commit/8e7efbcdab5aef167d85e6109ec76ddfcc92a682))

## [0.10.0](https://github.com/coderage-labs/spillway/compare/v0.9.0...v0.10.0) (2026-08-23)


### Features

* warn on login if daemon still holds the old credential ([#46](https://github.com/coderage-labs/spillway/issues/46)) ([#49](https://github.com/coderage-labs/spillway/issues/49)) ([633cd30](https://github.com/coderage-labs/spillway/commit/633cd30025368d7cc7b156b75970cd2c2ff2741f))


### Fixes

* a partial bind must not count as owning the callback port ([#52](https://github.com/coderage-labs/spillway/issues/52)) ([06b1168](https://github.com/coderage-labs/spillway/commit/06b1168219060d1d824f185d6890cdb785c05375))
* re-authenticating no longer wipes an account's label, priority and overage ([#50](https://github.com/coderage-labs/spillway/issues/50)) ([4492dea](https://github.com/coderage-labs/spillway/commit/4492dea98c122f770c3e1d8e4169f0968a77ccc1))
* share the account-name resolver across login, remove, priority, overage ([#44](https://github.com/coderage-labs/spillway/issues/44)) ([#47](https://github.com/coderage-labs/spillway/issues/47)) ([cbc15c1](https://github.com/coderage-labs/spillway/commit/cbc15c15573a5f7d22d12648633e64bc7ae1ca35))

## [0.9.0](https://github.com/coderage-labs/spillway/compare/v0.8.0...v0.9.0) (2026-08-23)


### Features

* per-family quota selection so a spent fable bucket doesn't gate Sonnet ([#24](https://github.com/coderage-labs/spillway/issues/24)) ([#43](https://github.com/coderage-labs/spillway/issues/43)) ([e71db0a](https://github.com/coderage-labs/spillway/commit/e71db0a03975e006217dcea2df74961166b11cb7))


### Documentation

* the session key is per session, not per machine ([#41](https://github.com/coderage-labs/spillway/issues/41)) ([9d71760](https://github.com/coderage-labs/spillway/commit/9d71760b8d3689f11b0ce55202258c63d84d952e))

## [0.8.0](https://github.com/coderage-labs/spillway/compare/v0.7.1...v0.8.0) (2026-08-22)


### Features

* resolve account names in the CLI, and let bare `switch` report ([#40](https://github.com/coderage-labs/spillway/issues/40)) ([4990090](https://github.com/coderage-labs/spillway/commit/4990090429b8b0e895dff95d7dcda827eafdd97d))


### Fixes

* **proxy:** make upstream HTTP/1.1 deliberate, not incidental ([#27](https://github.com/coderage-labs/spillway/issues/27)) ([#36](https://github.com/coderage-labs/spillway/issues/36)) ([3e26662](https://github.com/coderage-labs/spillway/commit/3e2666231590112c0944ebbe709fc3d9bd323dbb))
* **proxy:** rotate off upstream 5xx instead of streaming it to the client ([#26](https://github.com/coderage-labs/spillway/issues/26)) ([#37](https://github.com/coderage-labs/spillway/issues/37)) ([8b861ff](https://github.com/coderage-labs/spillway/commit/8b861ff08fd189fa00d6cfa3b1a74733d5b9e379))
* seed quota windows from quota_samples at startup ([#34](https://github.com/coderage-labs/spillway/issues/34)) ([#38](https://github.com/coderage-labs/spillway/issues/38)) ([b756cff](https://github.com/coderage-labs/spillway/commit/b756cff409c282514abe722f1249aa5c9d1de423))


### Internal

* stop TestEventsSSE racing the subscription ([#32](https://github.com/coderage-labs/spillway/issues/32)) ([bb3099c](https://github.com/coderage-labs/spillway/commit/bb3099c80c751ee504f48a0b1d83866fe0ef5f8f))


### Documentation

* fix four design-doc citations that point nowhere ([#39](https://github.com/coderage-labs/spillway/issues/39)) ([3743fa6](https://github.com/coderage-labs/spillway/commit/3743fa65540446e777648135eeba13afc6a04a86))
* **probe:** correct the design-doc citation and the probing schedule ([#30](https://github.com/coderage-labs/spillway/issues/30)) ([#33](https://github.com/coderage-labs/spillway/issues/33)) ([0b253c9](https://github.com/coderage-labs/spillway/commit/0b253c9250d029b20884c651692a30cc32ad7625))

## [0.7.1](https://github.com/coderage-labs/spillway/compare/v0.7.0...v0.7.1) (2026-08-22)


### Internal

* **docs:** guard the README commands table against the CLI dispatch ([#20](https://github.com/coderage-labs/spillway/issues/20)) ([#21](https://github.com/coderage-labs/spillway/issues/21)) ([12cbf09](https://github.com/coderage-labs/spillway/commit/12cbf096706a90435e2894c10b71741b6538d2a0))

## [0.7.0](https://github.com/coderage-labs/spillway/compare/v0.6.0...v0.7.0) (2026-08-22)


### Features

* pin the pool to a named account ([#18](https://github.com/coderage-labs/spillway/issues/18)) ([f5b08d2](https://github.com/coderage-labs/spillway/commit/f5b08d23a5e4c0cc1cd3703d20828b2972a9b26e))


### Fixes

* guard the pool fields the dashboard writes ([#16](https://github.com/coderage-labs/spillway/issues/16)) ([2254349](https://github.com/coderage-labs/spillway/commit/2254349471ba8ec8f117e901d34bf7f1b84b3026))
* **proxy:** record the mapped model on early-return responses too ([#14](https://github.com/coderage-labs/spillway/issues/14)) ([#15](https://github.com/coderage-labs/spillway/issues/15)) ([5cd2ade](https://github.com/coderage-labs/spillway/commit/5cd2adee0e99e026af095506f0db32a14ca24ce5))
* refuse a non-loopback proxy bind unless explicitly opted in ([#17](https://github.com/coderage-labs/spillway/issues/17)) ([df35f56](https://github.com/coderage-labs/spillway/commit/df35f56dea8f2cf3281d6daa54929455404188ba)), closes [#12](https://github.com/coderage-labs/spillway/issues/12)


### Documentation

* bring the README back in line with what the thing does ([ee01732](https://github.com/coderage-labs/spillway/commit/ee01732250a06ddc2faf5e0b005dd5fbaaceec98))
* document the pin ([#19](https://github.com/coderage-labs/spillway/issues/19)) ([583d267](https://github.com/coderage-labs/spillway/commit/583d267eefc21dbe73347d0a5ed5dcac47150f26))

## [0.6.0](https://github.com/coderage-labs/spillway/compare/v0.5.2...v0.6.0) (2026-08-22)


### Features

* the status line stays silent in a session that is not on spillway ([cff78c7](https://github.com/coderage-labs/spillway/commit/cff78c741f8c2baa0e892ea0c098371da144a95d))

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
