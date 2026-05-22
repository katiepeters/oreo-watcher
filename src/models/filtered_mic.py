import math
import struct
from typing import ClassVar, Mapping, Optional, Sequence, Tuple, cast

from typing_extensions import Self
from viam.components.audio_input import AudioInput
from viam.errors import NoCaptureToStoreError
from viam.media.audio import Audio
from viam.proto.app.robot import ComponentConfig
from viam.proto.common import ResourceName
from viam.resource.base import ResourceBase
from viam.resource.easy_resource import EasyResource
from viam.resource.types import Model, ModelFamily
from viam.streams import Stream, StreamWithIterator


class FilteredMic(AudioInput, EasyResource):
    MODEL: ClassVar[Model] = Model(
        ModelFamily("katie-viam", "oreo-watcher"), "filtered-mic"
    )

    underlying_mic: AudioInput
    min_db: float

    @classmethod
    def new(
        cls, config: ComponentConfig, dependencies: Mapping[ResourceName, ResourceBase]
    ) -> Self:
        return super().new(config, dependencies)

    @classmethod
    def validate_config(
        cls, config: ComponentConfig
    ) -> Tuple[Sequence[str], Sequence[str]]:
        underlying_mic_name = config.attributes.fields["underlying_mic"].string_value
        if underlying_mic_name == "":
            raise Exception("underlying_mic is a required FilteredMic attribute")
        return [underlying_mic_name], []

    def reconfigure(
        self, config: ComponentConfig, dependencies: Mapping[ResourceName, ResourceBase]
    ):
        underlying_mic_name = config.attributes.fields["underlying_mic"].string_value
        resource_name = ResourceName(
            namespace="rdk", type="component", subtype="audio_in", name=underlying_mic_name
        )
        self.underlying_mic = cast(AudioInput, dependencies[resource_name])

        fields = config.attributes.fields
        self.min_db = fields["min_db"].number_value if "min_db" in fields else -40.0

    def _chunk_to_db(self, audio: Audio) -> float:
        """Calculate dBFS from an Audio chunk (float32 interleaved format)."""
        data = audio.chunk.data
        num_samples = len(data) // 4  # float32 = 4 bytes
        if num_samples == 0:
            return -100.0
        samples = struct.unpack(f"{num_samples}f", data)
        mean_squared = sum(s * s for s in samples) / num_samples
        rms = math.sqrt(mean_squared)
        if rms == 0:
            return -100.0
        return 20.0 * math.log10(rms)

    async def stream(self, *, timeout: Optional[float] = None, **kwargs) -> Stream[Audio]:
        async def _read():
            kwargs_without_metadata = {k: v for k, v in kwargs.items() if k != "metadata"}
            underlying_stream = await self.underlying_mic.stream(timeout=timeout, **kwargs_without_metadata)

            # Read the first chunk to check the audio level
            first_chunk = None
            async for audio in underlying_stream:
                first_chunk = audio
                break

            if first_chunk is None:
                return

            db_level = self._chunk_to_db(first_chunk)
            self.logger.debug(f"Audio level: {db_level:.1f} dB (threshold: {self.min_db} dB)")

            if db_level < self.min_db:
                raise NoCaptureToStoreError()

            yield first_chunk
            async for audio in underlying_stream:
                yield audio

        return StreamWithIterator(_read())

    async def get_properties(
        self, *, timeout: Optional[float] = None, **kwargs
    ) -> AudioInput.Properties:
        kwargs_without_metadata = {k: v for k, v in kwargs.items() if k != "metadata"}
        return await self.underlying_mic.get_properties(timeout=timeout, **kwargs_without_metadata)
