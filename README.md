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

## Model katie-viam:oreo-watcher:mode-button

Sensor that lets a Shelly BLU RC Button 4 remote (including the ZB models, e.g. `SBBT-104CUS`) select the operating mode. After a mode is selected, the speaker says which mode is now active.

| Button | Action                                  |
|--------|-----------------------------------------|
| 1      | Switch to `home`, then say "Switched to home mode"            |
| 2      | Switch to `away_lite`, then say "Switched to away lite mode"  |
| 3      | Switch to `away`, then say "Switched to away mode"            |
| 4      | Say "Currently in ... mode" without changing it               |

A single, double, or triple press all count as a press. Long presses and holds are ignored. The mode is only spoken once `mode-controller` has accepted and persisted it, so hearing the announcement means the switch happened. Each announcement is preceded by a short two-note chime, which keeps the speaker from swallowing the first word as it wakes up.

The remote is heard over Bluetooth: every press is broadcast as a BTHome advertisement, which this component picks up through BlueZ. The remote needs no pairing and the machine needs no Zigbee radio, but it does need a Bluetooth adapter and BlueZ (Linux only), and the remote must be within Bluetooth range. The remote's BTHome encryption must be left off (the factory default).

### Configuration

```json
{
  "mac_address": "AA:BB:CC:DD:EE:FF",
  "mode_controller": "mode-controller",
  "bark_monitor": "bark-monitor"
}
```

#### Attributes

| Name              | Type   | Inclusion | Description |
|-------------------|--------|-----------|-------------|
| `mode_controller` | string | Required  | Name of the `mode-controller` sensor to drive |
| `bark_monitor`    | string | Required  | Name of the `bark-monitor` sensor; its `speaker` makes the announcements (and it mutes the mic while speaking) |
| `mac_address`     | string | Optional  | The remote's Bluetooth address. Presses are ignored until this is set — see below |
| `adapter`         | string | Optional  | BlueZ adapter to listen on. Defaults to `hci0` |
| `announcement_volume` | float | Optional | How loudly the mode is announced, above 0 and at most 1. Defaults to 1 (the speaker's own volume). Affects only these announcements, not bark warnings |

#### Finding the remote's address

Leave `mac_address` out, press any button on the remote, and read the address from the `discovered_remotes` reading (or the component's logs). Copy it into `mac_address`. Presses only take effect from the one configured remote.

### Readings

| Key                  | Type     | Description |
|----------------------|----------|-------------|
| `mac_address`        | string   | The configured remote, or `""` |
| `battery_percent`    | int      | Remote battery level, once it has been heard |
| `last_button`        | int      | Most recent button pressed on the remote (1-4) |
| `last_press_at`      | string   | RFC3339 time of that press |
| `bluetooth_error`    | string   | Present only while Bluetooth listening is failing (it retries every 10s) |
| `discovered_remotes` | string[] | Present only while `mac_address` is unset: addresses of button remotes heard |

### DoCommand

Simulate a press, e.g. to test the announcement without the remote in hand:

```json
{"command": "press", "button": 3}
```

Returns `{"mode": "away"}` after the announcement finishes, or an error if the mode could not be set or spoken.

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

## Model katie-viam:oreo-watcher:privacy-cam

Webcam that only opens the camera device while the machine is in `away` mode. In `home` and `away_lite` the device is closed, so nothing on the machine can get a frame from it: not the live feed in the Viam app, not `viam:viamrtsp:video-service` recording, not vision services, and not data capture.

The operating mode is read from the state file that `mode-controller` writes. Anything other than a readable `away` mode, including a missing or corrupt state file, keeps the camera off.

- **Entering `away`:** the device opens within about a second.
- **Leaving `away`:** the device closes within about a second, and any `Images` call made after the change fails, even before the device has closed.
- **While the camera is off:** `Images` and `NextPointCloud` return `camera is off: cameras are only enabled in away mode`.
  - A `viam:viamrtsp:video-service` on the camera keeps polling and logs a warning for each failed poll. rdk deduplicates these to about five lines a minute per service.
- **`Properties`** never opens the device. While the camera is off, it reports a camera with no intrinsics.

### Configuration

`privacy-cam` takes the same attributes as `rdk:builtin:webcam`, so you can switch an existing webcam over by changing only its `model`. Use a stable path from `/dev/v4l/by-id/` or `/dev/v4l/by-path/` for `video_path`: if it is empty, the camera opens the first webcam it finds, which may be a different camera on machines with more than one.

#### Attributes

| Name          | Type   | Inclusion   | Description                                                  |
|---------------|--------|-------------|--------------------------------------------------------------|
| `video_path`  | string | Recommended | Device to open, for example a `/dev/v4l/by-id/` name           |
| `format`      | string | Optional    | Pixel format, for example `MJPEG` or `YUYV`                   |
| `width_px`    | int    | Optional    | Frame width                                                  |
| `height_px`   | int    | Optional    | Frame height                                                 |
| `frame_rate`  | float  | Optional    | Frames per second                                            |

Any other `rdk:builtin:webcam` attribute, such as `intrinsic_parameters` or `distortion_parameters`, also works.

#### Example Configuration

```json
{
  "name": "cam",
  "api": "rdk:component:camera",
  "model": "katie-viam:oreo-watcher:privacy-cam",
  "attributes": {
    "video_path": "usb-EMEET_EMEET_SmartCam_C950_24082114152-video-index0",
    "format": "MJPEG",
    "width_px": 640,
    "height_px": 480
  }
}
```

### DoCommand

Any command returns whether the device is open right now. This is useful for checking the camera is really off outside `away`.

```json
{}
```

Response:

```json
{"device_open": false}
```

### Limits

`privacy-cam` controls only its own device. A shell service, or another camera component configured on the same device, can still open it directly.
