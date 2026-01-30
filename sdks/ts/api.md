# Health

Types:

- <code><a href="./src/resources/health.ts">HealthCheckResponse</a></code>

Methods:

- <code title="get /health">client.health.<a href="./src/resources/health.ts">check</a>() -> HealthCheckResponse</code>

# Images

Types:

- <code><a href="./src/resources/images.ts">Image</a></code>
- <code><a href="./src/resources/images.ts">ImageListResponse</a></code>

Methods:

- <code title="post /images">client.images.<a href="./src/resources/images.ts">create</a>({ ...params }) -> Image</code>
- <code title="get /images">client.images.<a href="./src/resources/images.ts">list</a>() -> ImageListResponse</code>
- <code title="delete /images/{name}">client.images.<a href="./src/resources/images.ts">delete</a>(name) -> void</code>
- <code title="get /images/{name}">client.images.<a href="./src/resources/images.ts">get</a>(name) -> Image</code>

# Instances

Types:

- <code><a href="./src/resources/instances/instances.ts">Instance</a></code>
- <code><a href="./src/resources/instances/instances.ts">PathInfo</a></code>
- <code><a href="./src/resources/instances/instances.ts">PortMapping</a></code>
- <code><a href="./src/resources/instances/instances.ts">VolumeMount</a></code>
- <code><a href="./src/resources/instances/instances.ts">InstanceListResponse</a></code>
- <code><a href="./src/resources/instances/instances.ts">InstanceLogsResponse</a></code>

Methods:

- <code title="post /instances">client.instances.<a href="./src/resources/instances/instances.ts">create</a>({ ...params }) -> Instance</code>
- <code title="get /instances">client.instances.<a href="./src/resources/instances/instances.ts">list</a>() -> InstanceListResponse</code>
- <code title="delete /instances/{id}">client.instances.<a href="./src/resources/instances/instances.ts">delete</a>(id) -> void</code>
- <code title="get /instances/{id}">client.instances.<a href="./src/resources/instances/instances.ts">get</a>(id) -> Instance</code>
- <code title="get /instances/{id}/logs">client.instances.<a href="./src/resources/instances/instances.ts">logs</a>(id, { ...params }) -> string</code>
- <code title="post /instances/{id}/restore">client.instances.<a href="./src/resources/instances/instances.ts">restore</a>(id) -> Instance</code>
- <code title="post /instances/{id}/standby">client.instances.<a href="./src/resources/instances/instances.ts">standby</a>(id) -> Instance</code>
- <code title="post /instances/{id}/start">client.instances.<a href="./src/resources/instances/instances.ts">start</a>(id) -> Instance</code>
- <code title="get /instances/{id}/stat">client.instances.<a href="./src/resources/instances/instances.ts">stat</a>(id, { ...params }) -> PathInfo</code>
- <code title="post /instances/{id}/stop">client.instances.<a href="./src/resources/instances/instances.ts">stop</a>(id) -> Instance</code>

## Volumes

Methods:

- <code title="post /instances/{id}/volumes/{volumeId}">client.instances.volumes.<a href="./src/resources/instances/volumes.ts">attach</a>(volumeID, { ...params }) -> Instance</code>
- <code title="delete /instances/{id}/volumes/{volumeId}">client.instances.volumes.<a href="./src/resources/instances/volumes.ts">detach</a>(volumeID, { ...params }) -> Instance</code>

# Volumes

Types:

- <code><a href="./src/resources/volumes.ts">Volume</a></code>
- <code><a href="./src/resources/volumes.ts">VolumeAttachment</a></code>
- <code><a href="./src/resources/volumes.ts">VolumeListResponse</a></code>

Methods:

- <code title="post /volumes">client.volumes.<a href="./src/resources/volumes.ts">create</a>({ ...params }) -> Volume</code>
- <code title="get /volumes">client.volumes.<a href="./src/resources/volumes.ts">list</a>() -> VolumeListResponse</code>
- <code title="delete /volumes/{id}">client.volumes.<a href="./src/resources/volumes.ts">delete</a>(id) -> void</code>
- <code title="post /volumes/from-archive">client.volumes.<a href="./src/resources/volumes.ts">createFromArchive</a>(body, { ...params }) -> Volume</code>
- <code title="get /volumes/{id}">client.volumes.<a href="./src/resources/volumes.ts">get</a>(id) -> Volume</code>

# Devices

Types:

- <code><a href="./src/resources/devices.ts">AvailableDevice</a></code>
- <code><a href="./src/resources/devices.ts">Device</a></code>
- <code><a href="./src/resources/devices.ts">DeviceType</a></code>
- <code><a href="./src/resources/devices.ts">DeviceListResponse</a></code>
- <code><a href="./src/resources/devices.ts">DeviceListAvailableResponse</a></code>

Methods:

- <code title="post /devices">client.devices.<a href="./src/resources/devices.ts">create</a>({ ...params }) -> Device</code>
- <code title="get /devices/{id}">client.devices.<a href="./src/resources/devices.ts">retrieve</a>(id) -> Device</code>
- <code title="get /devices">client.devices.<a href="./src/resources/devices.ts">list</a>() -> DeviceListResponse</code>
- <code title="delete /devices/{id}">client.devices.<a href="./src/resources/devices.ts">delete</a>(id) -> void</code>
- <code title="get /devices/available">client.devices.<a href="./src/resources/devices.ts">listAvailable</a>() -> DeviceListAvailableResponse</code>

# Ingresses

Types:

- <code><a href="./src/resources/ingresses.ts">Ingress</a></code>
- <code><a href="./src/resources/ingresses.ts">IngressMatch</a></code>
- <code><a href="./src/resources/ingresses.ts">IngressRule</a></code>
- <code><a href="./src/resources/ingresses.ts">IngressTarget</a></code>
- <code><a href="./src/resources/ingresses.ts">IngressListResponse</a></code>

Methods:

- <code title="post /ingresses">client.ingresses.<a href="./src/resources/ingresses.ts">create</a>({ ...params }) -> Ingress</code>
- <code title="get /ingresses">client.ingresses.<a href="./src/resources/ingresses.ts">list</a>() -> IngressListResponse</code>
- <code title="delete /ingresses/{id}">client.ingresses.<a href="./src/resources/ingresses.ts">delete</a>(id) -> void</code>
- <code title="get /ingresses/{id}">client.ingresses.<a href="./src/resources/ingresses.ts">get</a>(id) -> Ingress</code>

# Resources

Types:

- <code><a href="./src/resources/resources.ts">DiskBreakdown</a></code>
- <code><a href="./src/resources/resources.ts">GPUProfile</a></code>
- <code><a href="./src/resources/resources.ts">GPUResourceStatus</a></code>
- <code><a href="./src/resources/resources.ts">PassthroughDevice</a></code>
- <code><a href="./src/resources/resources.ts">ResourceAllocation</a></code>
- <code><a href="./src/resources/resources.ts">ResourceStatus</a></code>
- <code><a href="./src/resources/resources.ts">Resources</a></code>

Methods:

- <code title="get /resources">client.resources.<a href="./src/resources/resources.ts">get</a>() -> Resources</code>

# Builds

Types:

- <code><a href="./src/resources/builds.ts">Build</a></code>
- <code><a href="./src/resources/builds.ts">BuildEvent</a></code>
- <code><a href="./src/resources/builds.ts">BuildPolicy</a></code>
- <code><a href="./src/resources/builds.ts">BuildProvenance</a></code>
- <code><a href="./src/resources/builds.ts">BuildStatus</a></code>
- <code><a href="./src/resources/builds.ts">BuildListResponse</a></code>

Methods:

- <code title="post /builds">client.builds.<a href="./src/resources/builds.ts">create</a>({ ...params }) -> Build</code>
- <code title="get /builds">client.builds.<a href="./src/resources/builds.ts">list</a>() -> BuildListResponse</code>
- <code title="delete /builds/{id}">client.builds.<a href="./src/resources/builds.ts">cancel</a>(id) -> void</code>
- <code title="get /builds/{id}/events">client.builds.<a href="./src/resources/builds.ts">events</a>(id, { ...params }) -> BuildEvent</code>
- <code title="get /builds/{id}">client.builds.<a href="./src/resources/builds.ts">get</a>(id) -> Build</code>
