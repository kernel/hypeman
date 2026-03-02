# Changelog

## 0.14.0 (2026-03-02)

Full Changelog: [v0.13.0...v0.14.0](https://github.com/kernel/hypeman/compare/v0.13.0...v0.14.0)

### Features

* add boot time optimizations for faster VM startup ([3992761](https://github.com/kernel/hypeman/commit/3992761e3ad8ebb0cc22fb7408199b068e9d8013))
* add CpToInstance and CpFromInstance functions ([00c6817](https://github.com/kernel/hypeman/commit/00c6817dee742734dbfe64a4266ba2acd15b42c9))
* add hypeman cp for file copy to/from running VMs ([49ea898](https://github.com/kernel/hypeman/commit/49ea89852eed5e0893febc4c68d295a0d1a8bfe5))
* Add image_name parameter to builds ([fbe8fea](https://github.com/kernel/hypeman/commit/fbe8fea39bf6df79cc05a64de9db7d397387330a))
* add metadata and state filtering to GET /instances ([d017749](https://github.com/kernel/hypeman/commit/d017749192878486e76e4370d6f3eff9bfe18494))
* Add metadata field to instances ([8ce4014](https://github.com/kernel/hypeman/commit/8ce40145c0f6db8e928efa9c2909521a6d452579))
* add Push and PushImage functions for OCI registry push ([7417cc8](https://github.com/kernel/hypeman/commit/7417cc8a56c7d11c535ac7ab9a7b3d21d80bd2b4))
* Add to stainless config new API endpoints ([de008e8](https://github.com/kernel/hypeman/commit/de008e89fadbaedde6554181618fb03c71b49465))
* Add vGPU support ([7f330c1](https://github.com/kernel/hypeman/commit/7f330c1e7990f417e593214350200acd5b371d84))
* **api:** add exec ([f3992ff](https://github.com/kernel/hypeman/commit/f3992ffe807e7006a25ae2211cd5cb25fb599bff))
* **api:** add stat endpoint and PathInfo error field ([7bf7205](https://github.com/kernel/hypeman/commit/7bf7205979e32bb0ca4b9e8501e3368bd21ee73b))
* **api:** manual updates ([f60e600](https://github.com/kernel/hypeman/commit/f60e60015bb9ce18c7083963d9ecd11c980de495))
* Better stop behavior ([12cfb4e](https://github.com/kernel/hypeman/commit/12cfb4e4ddfc198c65b09efeb73c8db0fd609f5f))
* **builds:** implement two-tier build cache with per-repo token scopes ([0e29d03](https://github.com/kernel/hypeman/commit/0e29d03d94cf50a0d0e83c323f7ed9f2e15f3e61))
* **client:** add a convenient param.SetJSON helper ([7fea166](https://github.com/kernel/hypeman/commit/7fea1660f3d17d8a35f5d2f6aa352b553785624b))
* Disable default hotplug memory allocation ([291b2d7](https://github.com/kernel/hypeman/commit/291b2d7d962ccf94b5a328f213750e2f5fb5dca7))
* **encoder:** support bracket encoding form-data object members ([8ab31e8](https://github.com/kernel/hypeman/commit/8ab31e89c70baa967842c1c160d0b49db44b089a))
* Generate log streaming ([f444c22](https://github.com/kernel/hypeman/commit/f444c22bd9eb0ad06e66b3ca167171ddec2836e4))
* gpu passthrough ([067a01b](https://github.com/kernel/hypeman/commit/067a01b4ac06e82c2db6b165127144afa18a691d))
* Ingress ([c751d1a](https://github.com/kernel/hypeman/commit/c751d1a6bba5ca619c03f833f27251c6d3b855a7))
* Initialize volume with data ([32d4047](https://github.com/kernel/hypeman/commit/32d404746df0a3e9d83e7651105e6c6daa16476f))
* Network manager ([8f772c7](https://github.com/kernel/hypeman/commit/8f772c7deca75a76fef06c9b8e4d98c05769e590))
* Operational logs over API: hypeman.log, vmm.log ([27fa1e7](https://github.com/kernel/hypeman/commit/27fa1e76a4a7f512b40fdd18e87a4dcc448a60b2))
* QEMU support ([5ad4fd3](https://github.com/kernel/hypeman/commit/5ad4fd34cd66a60aa903f2ea80a1b16b8d75792b))
* QEMU support ([d708091](https://github.com/kernel/hypeman/commit/d70809169d136df3f1efbf961f2a90084e1f9fa5))
* Remove exec from openapi spec ([ee8d1bb](https://github.com/kernel/hypeman/commit/ee8d1bb586a130c0b6629603ca4edb489f671889))
* Resource accounting ([28f7c10](https://github.com/kernel/hypeman/commit/28f7c108d3acb984fdce2dd161b657ed7981a53e))
* Resource accounting ([4141287](https://github.com/kernel/hypeman/commit/414128770e8137ed2a40d404f0f4ac06ea1a0731))
* Start and Stop VM ([84e384c](https://github.com/kernel/hypeman/commit/84e384c21243bd0227178f3479f94b9d50ded6d0))
* Support TLS for ingress ([dc370b5](https://github.com/kernel/hypeman/commit/dc370b599b595fdfaf96cfb9c3e65aeaa1c85e46))
* try to fix name collision in codegen ([8173a73](https://github.com/kernel/hypeman/commit/8173a73d0317d35870d5a3cec8f3fdec56fcf362))
* Use resources module for input validation ([313413e](https://github.com/kernel/hypeman/commit/313413e29480f4c3df8f5e0a867fba6ff44f5161))
* Volume readonly multi-attach ([bac3fd2](https://github.com/kernel/hypeman/commit/bac3fd2cee3325dc3d1b31e6077ad1f1ce13340c))
* Volumes ([099f9b8](https://github.com/kernel/hypeman/commit/099f9b8a2553087e117c8c8a9731900081d713f0))
* wire up memory_mb and cpus in builds API ([271f4c8](https://github.com/kernel/hypeman/commit/271f4c8c4caa0ac6910bdb9f396bcf1fd875eb29))


### Bug Fixes

* allow canceling a request while it is waiting to retry ([adcf40b](https://github.com/kernel/hypeman/commit/adcf40b9e4a27438b72ce755e49640681baf0ec0))
* **client:** correctly specify Accept header with */* instead of empty ([aab8e47](https://github.com/kernel/hypeman/commit/aab8e471488b5c251fe0bd6be185b8cbc4fd5905))
* **docs:** add missing pointer prefix to api.md return types ([1b4e67e](https://github.com/kernel/hypeman/commit/1b4e67e881b60205c89bec005a195f378eec2c9a))
* enable Windows cross-compilation ([fd74c45](https://github.com/kernel/hypeman/commit/fd74c457f9a6a987cdadf6e90956b2eaad0cc624))
* **encoder:** correctly serialize NullStruct ([b1be988](https://github.com/kernel/hypeman/commit/b1be988cd6a605d13f7039bd56e5f5d3216d8d8f))
* incorrect reporting of Stopped, add better error reporting ([c475606](https://github.com/kernel/hypeman/commit/c475606387687d46c762c1b4b7e1471561363d1f))
* **mcp:** correct code tool API endpoint ([ed2e6d5](https://github.com/kernel/hypeman/commit/ed2e6d58c880ca66041492e7be16022eba2fe350))
* rename param to avoid collision ([335c8d3](https://github.com/kernel/hypeman/commit/335c8d37b7ba01be45caa888a68160370f46b7c4))
* send query params for NewFromArchive ([a8c45a6](https://github.com/kernel/hypeman/commit/a8c45a69e83c96137c772d999a115972f0e6a003))
* skip usage tests that don't work with Prism ([d62b246](https://github.com/kernel/hypeman/commit/d62b2466715247e7d083ab7ef33040e5da036bd8))


### Chores

* add float64 to valid types for RegisterFieldValidator ([b4666fd](https://github.com/kernel/hypeman/commit/b4666fd1bfcdd17b0a4d4bf88541670cd40c8b1c))
* elide duplicate aliases ([ddd0a06](https://github.com/kernel/hypeman/commit/ddd0a06031e4e4dc41c905d99a938261d8ca270b))
* **internal:** codegen related update ([80f3302](https://github.com/kernel/hypeman/commit/80f330283f28d00c8841de06104fe5e245e681c9))
* **internal:** move custom custom `json` tags to `api` ([1fe0edd](https://github.com/kernel/hypeman/commit/1fe0eddb1920ef8fbb49ed764de1ed21cfcd59f6))
* **internal:** remove mock server code ([c09068f](https://github.com/kernel/hypeman/commit/c09068f8f55140308a725b4bb81dd947183945ca))
* **internal:** update `actions/checkout` version ([7074484](https://github.com/kernel/hypeman/commit/7074484b88cc6e360a360a9c18725da807b57c18))
* rename GitHub org from onkernel to kernel ([31d8772](https://github.com/kernel/hypeman/commit/31d87723c129bc4c616991a551072ff008827672))
* **stainless:** update SDKs for PR [#116](https://github.com/kernel/hypeman/issues/116) ([560f851](https://github.com/kernel/hypeman/commit/560f8512392ffda9d3f478494b593bda363b1c86))
* update mock server docs ([7286a2c](https://github.com/kernel/hypeman/commit/7286a2c6eab2c542c34767faaaccd0124ebcebbc))
* update SDK settings ([66c7ac5](https://github.com/kernel/hypeman/commit/66c7ac5d8e9106c6f9034e3ce87f31c4e0470620))


### Refactors

* cross-platform foundation for macOS support ([e0036b9](https://github.com/kernel/hypeman/commit/e0036b92a152f8600f75981f8c841cba077ebc10))

## 0.1.0 (2025-12-23)

Full Changelog: [v0.0.2...v0.1.0](https://github.com/onkernel/hypeman-ts/compare/v0.0.2...v0.1.0)

### Features

* add cpToInstance and cpFromInstance functions ([113b412](https://github.com/onkernel/hypeman-ts/commit/113b4129c3bcca7f2ba8cc1c590542a00ad873a0))
* add hypeman cp for file copy to/from running VMs ([0ee9f4a](https://github.com/onkernel/hypeman-ts/commit/0ee9f4a86ea8a6a94b896ce66cf7dc90a8d7c296))
* **api:** add autogenerated stat endpoint from Stainless ([38c9dbd](https://github.com/onkernel/hypeman-ts/commit/38c9dbdf9a129b434b7c9246d321da6b6f9ed2d4))
* QEMU support ([8f0f4f4](https://github.com/onkernel/hypeman-ts/commit/8f0f4f4b0cf1723af0cd79e549f4aacaa9c6584b))


### Bug Fixes

* **cp:** address bugbot review comments ([ba6c287](https://github.com/onkernel/hypeman-ts/commit/ba6c2876049bae125951dbe363f5d6deabaaf03f))


### Chores

* sync repo ([75f4200](https://github.com/onkernel/hypeman-ts/commit/75f4200bb4c22d85be3ab8b065078ee1165cfa22))

## 0.0.2 (2025-12-22)

Full Changelog: [v0.0.1...v0.0.2](https://github.com/onkernel/hypeman-ts/compare/v0.0.1...v0.0.2)

### Chores

* configure new SDK language ([9eba465](https://github.com/onkernel/hypeman-ts/commit/9eba465b84418dc70994a43eb6b28e498841d8a6))
* update SDK settings ([8a8e921](https://github.com/onkernel/hypeman-ts/commit/8a8e92120eea2a6cd22182555ec2caa2d159fca0))
* update SDK settings ([523dcfd](https://github.com/onkernel/hypeman-ts/commit/523dcfdff90593e47211b52a05f8d3bed1433780))
