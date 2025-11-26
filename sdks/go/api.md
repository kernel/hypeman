# Health

Response Types:

- <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#HealthCheckResponse">HealthCheckResponse</a>

Methods:

- <code title="get /health">client.Health.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#HealthService.Check">Check</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#HealthCheckResponse">HealthCheckResponse</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>

# Images

Response Types:

- <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Image">Image</a>

Methods:

- <code title="post /images">client.Images.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#ImageService.New">New</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, body <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#ImageNewParams">ImageNewParams</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Image">Image</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /images/{name}">client.Images.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#ImageService.Get">Get</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, name <a href="https://pkg.go.dev/builtin#string">string</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Image">Image</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /images">client.Images.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#ImageService.List">List</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>) ([]<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Image">Image</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="delete /images/{name}">client.Images.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#ImageService.Delete">Delete</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, name <a href="https://pkg.go.dev/builtin#string">string</a>) <a href="https://pkg.go.dev/builtin#error">error</a></code>

# Instances

Response Types:

- <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>

Methods:

- <code title="post /instances">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.New">New</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, body <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceNewParams">InstanceNewParams</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /instances/{id}">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.Get">Get</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /instances">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.List">List</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>) ([]<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="delete /instances/{id}">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.Delete">Delete</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) <a href="https://pkg.go.dev/builtin#error">error</a></code>
- <code title="post /instances/{id}/standby">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.PutInStandby">PutInStandby</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="post /instances/{id}/restore">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.RestoreFromStandby">RestoreFromStandby</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /instances/{id}/logs">client.Instances.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceService.StreamLogs">StreamLogs</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>, query <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceStreamLogsParams">InstanceStreamLogsParams</a>) (<a href="https://pkg.go.dev/builtin#string">string</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>

## Volumes

Methods:

- <code title="post /instances/{id}/volumes/{volumeId}">client.Instances.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceVolumeService.Attach">Attach</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, volumeID <a href="https://pkg.go.dev/builtin#string">string</a>, params <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceVolumeAttachParams">InstanceVolumeAttachParams</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="delete /instances/{id}/volumes/{volumeId}">client.Instances.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceVolumeService.Detach">Detach</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, volumeID <a href="https://pkg.go.dev/builtin#string">string</a>, body <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#InstanceVolumeDetachParams">InstanceVolumeDetachParams</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Instance">Instance</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>

# Volumes

Response Types:

- <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Volume">Volume</a>

Methods:

- <code title="post /volumes">client.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#VolumeService.New">New</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, body <a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#VolumeNewParams">VolumeNewParams</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Volume">Volume</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /volumes/{id}">client.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#VolumeService.Get">Get</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) (<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Volume">Volume</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="get /volumes">client.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#VolumeService.List">List</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>) ([]<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go">hypeman</a>.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#Volume">Volume</a>, <a href="https://pkg.go.dev/builtin#error">error</a>)</code>
- <code title="delete /volumes/{id}">client.Volumes.<a href="https://pkg.go.dev/github.com/onkernel/hypeman-go#VolumeService.Delete">Delete</a>(ctx <a href="https://pkg.go.dev/context">context</a>.<a href="https://pkg.go.dev/context#Context">Context</a>, id <a href="https://pkg.go.dev/builtin#string">string</a>) <a href="https://pkg.go.dev/builtin#error">error</a></code>
