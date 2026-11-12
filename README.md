# Module oreo-watcher 

Provide a description of the purpose of the module and any relevant information.

## Model katie-viam:oreo-watcher:motion-cam

Provide a description of the model and any relevant information.

### Configuration
The following attribute template can be used to configure this model:

```json
{
"attribute_1": <float>,
"attribute_2": <string>
}
```

#### Attributes

The following attributes are available for this model:

| Name          | Type   | Inclusion | Description                |
|---------------|--------|-----------|----------------------------|
| `attribute_1` | float  | Required  | Description of attribute 1 |
| `attribute_2` | string | Optional  | Description of attribute 2 |

#### Example Configuration

```json
{
  "attribute_1": 1.0,
  "attribute_2": "foo"
}
```

### DoCommand

If your model implements DoCommand, provide an example payload of each command that is supported and the arguments that can be used. If your model does not implement DoCommand, remove this section.

#### Example DoCommand

```json
{
  "command_name": {
    "arg1": "foo",
    "arg2": 1
  }
}
```

## Model katie-viam:oreo-watcher:inference-monitor

Sensor that reports how long each `bark-monitor` spectrogram classification call took. `bark-monitor` records the duration of every real classification it runs (calls where `spectrogram-cam` actually had a new capture to classify — calls that error out immediately because there was nothing new are not counted); this sensor drains those recorded samples on each poll and returns each as its own object, so no individual call's timing is lost even if this sensor is polled less often than classifications happen.

### Configuration

This model takes no attributes.

#### Example Configuration

```json
{
  "name": "inference-monitor",
  "api": "rdk:component:sensor",
  "model": "katie-viam:oreo-watcher:inference-monitor",
  "attributes": {}
}
```

### Readings

| Key       | Type   | Description                                                  |
|-----------|--------|----------------------------------------------------------------|
| `count`   | int    | Number of classification calls recorded since the last poll  |
| `samples` | object[] | One object per call, oldest first — see below              |

Each entry in `samples`:

| Key           | Type   | Description                                                                                   |
|---------------|--------|-------------------------------------------------------------------------------------------------|
| `started_at`  | string | RFC3339 timestamp of when the classification *call itself* began — not when the underlying audio was captured by the mic |
| `duration_ms` | float  | How long that call took, in milliseconds                                                       |

Example:

```json
{
  "count": 2,
  "samples": [
    {"started_at": "2026-08-06T14:03:11.482193-07:00", "duration_ms": 42.7},
    {"started_at": "2026-08-06T14:03:41.510902-07:00", "duration_ms": 39.1}
  ]
}
```

If no classification calls happened since the last poll, `Readings()` returns `ErrNoCaptureToStore` and nothing is captured for that poll.
