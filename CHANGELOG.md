# Changelog

## [0.3.0](https://github.com/abinnovision/hatchet-controller/compare/v0.2.0...v0.3.0) (2026-10-22)


### Features

* require hmac auth for Stripe, Slack and Svix webhooks ([#23](https://github.com/abinnovision/hatchet-controller/issues/23)) ([fb93373](https://github.com/abinnovision/hatchet-controller/commit/fb9337353f90d726a4f4d14400dd46d89392bf0d))


### Bug Fixes

* watch Secrets referenced by HatchetTenant webhooks ([#21](https://github.com/abinnovision/hatchet-controller/issues/21)) ([0be0c2b](https://github.com/abinnovision/hatchet-controller/commit/0be0c2b9cfb95683bcb2a06ade732182dfa15176))

## [0.2.0](https://github.com/abinnovision/hatchet-controller/compare/v0.1.0...v0.2.0) (2026-10-20)


### Features

* manage incoming webhooks declaratively on HatchetTenant ([#18](https://github.com/abinnovision/hatchet-controller/issues/18)) ([f7fbfab](https://github.com/abinnovision/hatchet-controller/commit/f7fbfabd6b025f131d800b7a6a82d2f615af3eec))
* manage tenant members declaratively on HatchetTenant ([#19](https://github.com/abinnovision/hatchet-controller/issues/19)) ([cf77deb](https://github.com/abinnovision/hatchet-controller/commit/cf77debbc675196dae5262400c9e84dd050e2644))
* sync the Hatchet tenant name, defaulting to &lt;namespace&gt;/&lt;name&gt; ([#20](https://github.com/abinnovision/hatchet-controller/issues/20)) ([09f5ac5](https://github.com/abinnovision/hatchet-controller/commit/09f5ac5f8145597609bb8e059a6e3435c45d46d5))


### Bug Fixes

* point the kustomize base at the released 0.1.0 image ([#14](https://github.com/abinnovision/hatchet-controller/issues/14)) ([ba66a2d](https://github.com/abinnovision/hatchet-controller/commit/ba66a2dffe39b05a2025be7b06be648a7f526e5a))

## 0.1.0 (2026-10-17)


### Features

* initial hatchet-controller ([97910a2](https://github.com/abinnovision/hatchet-controller/commit/97910a2d3f8294a09ccd7e2dce60ff0b5e8712f4))
* raise default token ttl to 8760h ([#10](https://github.com/abinnovision/hatchet-controller/issues/10)) ([5c12a6e](https://github.com/abinnovision/hatchet-controller/commit/5c12a6e1d14d06d5f6a746db9246ecb3a0faec75))


### Bug Fixes

* **deps:** bump the go-dependencies group with 3 updates ([#7](https://github.com/abinnovision/hatchet-controller/issues/7)) ([9484aa9](https://github.com/abinnovision/hatchet-controller/commit/9484aa941ec35ce93f42d76ba417e9e5401713cd))
