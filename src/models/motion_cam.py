import sys
import asyncio
import math
import statistics
from typing import (Any, ClassVar, Dict, Final, List, Mapping, Optional,
                    Sequence, Tuple, cast)

import sounddevice as sd

from typing_extensions import Self
from viam.components.camera import Camera
from viam.services.vision import Vision
from viam.media.video import NamedImage, ViamImage
from viam.proto.app.robot import ComponentConfig
from viam.proto.common import Geometry, ResourceName, ResponseMetadata
from viam.proto.component.camera import GetPropertiesResponse
from viam.resource.base import ResourceBase
from viam.resource.easy_resource import EasyResource
from viam.resource.types import Model, ModelFamily
from viam.utils import ValueTypes, from_dm_from_extra
from viam.errors import NoCaptureToStoreError


class MotionCam(Camera, EasyResource):
    # To enable debug-level logging, either run viam-server with the --debug option,
    # or configure your resource/machine to display debug logs.
    MODEL: ClassVar[Model] = Model(
        ModelFamily("katie-viam", "oreo-watcher"), "motion-cam"
    )

    @classmethod
    def new(
        cls, config: ComponentConfig, dependencies: Mapping[ResourceName, ResourceBase]
    ) -> Self:
        """This method creates a new instance of this Camera component.
        The default implementation sets the name from the `config` parameter and then calls `reconfigure`.

        Args:
            config (ComponentConfig): The configuration for this resource
            dependencies (Mapping[ResourceName, ResourceBase]): The dependencies (both required and optional)

        Returns:
            Self: The resource
        """
        return super().new(config, dependencies)

    @classmethod
    def validate_config(
        cls, config: ComponentConfig
    ) -> Tuple[Sequence[str], Sequence[str]]:
        """This method allows you to validate the configuration object received from the machine,
        as well as to return any required dependencies or optional dependencies based on that `config`.

        Args:
            config (ComponentConfig): The configuration for this resource

        Returns:
            Tuple[Sequence[str], Sequence[str]]: A tuple where the
                first element is a list of required dependencies and the
                second element is a list of optional dependencies
        """
        underlying_cam_name = config.attributes.fields["underlying_cam"].string_value
        if underlying_cam_name == "":
            raise Exception("underlying_cam is a required Oreo MotionCam attribute")
        
        vision_service_name = config.attributes.fields["vision_service"].string_value
        if vision_service_name == "":
            raise Exception("vision_service is a required Oreo MotionCam attribute")
        
        return [underlying_cam_name, vision_service_name], []

    def reconfigure(
        self, config: ComponentConfig, dependencies: Mapping[ResourceName, ResourceBase]
    ):
        """This method allows you to dynamically update your service when it receives a new `config` object.

        Args:
            config (ComponentConfig): The new configuration
            dependencies (Mapping[ResourceName, ResourceBase]): Any dependencies (both required and optional)
        """
        underlying_cam_name = config.attributes.fields["underlying_cam"].string_value
        underlying_cam = dependencies[Camera.get_resource_name(underlying_cam_name)]

        vision_service_name = config.attributes.fields["vision_service"].string_value
        vision_service = dependencies[Vision.get_resource_name(vision_service_name)]

        self.underlying_cam = cast(Camera, underlying_cam)
        self.vision_service = cast(Vision, vision_service)

    async def _has_noise(self, duration: float = 0.1, threshold: float = 0.01) -> bool:
        """Check if there's noise/audio activity from the microphone.
        
        Args:
            duration: Duration in seconds to sample audio
            threshold: RMS threshold above which we consider it "noise" (0.0 to 1.0)
            
        Returns:
            True if noise detected, False otherwise
        """
        try:
            sample_rate = 44100
            frames = int(duration * sample_rate)
            
            # Run blocking audio recording in executor to avoid blocking async
            loop = asyncio.get_event_loop()
            
            def _record():
                audio = sd.rec(frames, samplerate=sample_rate, channels=1, dtype='float32')
                sd.wait()
                return audio
            
            audio_data = await loop.run_in_executor(None, _record)
            
            # Calculate RMS (Root Mean Square) to measure audio level
            # Convert numpy array to list if needed, then calculate RMS
            audio_list = audio_data.flatten().tolist() if hasattr(audio_data, 'flatten') else list(audio_data)
            squared = [x * x for x in audio_list]
            mean_squared = statistics.mean(squared)
            rms = math.sqrt(mean_squared)
            
            return rms > threshold
        except Exception as e:
            self.logger.warning(f"Error checking for noise: {e}")
            return False

    async def get_image(
        self,
        mime_type: str = "",
        *,
        extra: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = None,
        **kwargs
    ) -> ViamImage:
        """Filters the output of the underlying camera"""
        # Remove metadata from kwargs as it's an internal RPC concern
        kwargs_without_metadata = {k: v for k, v in kwargs.items() if k != "metadata"}
        img = await self.underlying_cam.get_image(
            mime_type=mime_type,
            extra=extra,
            timeout=timeout,
            **kwargs_without_metadata
        )
        if from_dm_from_extra(extra):
            detections = await self.vision_service.get_detections(img)
            if len(detections) == 0:
                raise NoCaptureToStoreError()

        return img

    async def get_images(
        self,
        *,
        filter_source_names: Optional[Sequence[str]] = None,
        extra: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = None,
        **kwargs
    ) -> Tuple[Sequence[NamedImage], ResponseMetadata]:
        """Filters the output of the underlying camera"""
        # Remove metadata from kwargs as it's an internal RPC concern
        kwargs_without_metadata = {k: v for k, v in kwargs.items() if k != "metadata"}
        images, metadata = await self.underlying_cam.get_images(
            filter_source_names=filter_source_names,
            extra=extra,
            timeout=timeout,
            **kwargs_without_metadata
        )

        # Check for noise from microphone
        has_noise = await self._has_noise()
        if has_noise:
            self.logger.debug("Noise detected from microphone")
        else:
            return [], metadata

        if from_dm_from_extra(extra):
            # Filter images that have detections
            filtered_images = []
            for image in images:
                detections = await self.vision_service.get_detections(image.image)
                if len(detections) > 0:
                    filtered_images.append(image)
            images = filtered_images

        return images, metadata

    async def get_point_cloud(
        self,
        *,
        extra: Optional[Dict[str, Any]] = None,
        timeout: Optional[float] = None,
        **kwargs
    ) -> Tuple[bytes, str]:
        self.logger.error("`get_point_cloud` is not implemented")
        raise NotImplementedError()

    async def get_properties(
        self, *, timeout: Optional[float] = None, **kwargs
    ) -> Camera.Properties:
        # Remove metadata from kwargs as it's an internal RPC concern
        kwargs_without_metadata = {k: v for k, v in kwargs.items() if k != "metadata"}
        return await self.underlying_cam.get_properties(
            timeout=timeout,
            **kwargs_without_metadata
        )

    async def do_command(
        self,
        command: Mapping[str, ValueTypes],
        *,
        timeout: Optional[float] = None,
        **kwargs
    ) -> Mapping[str, ValueTypes]:
        self.logger.error("`do_command` is not implemented")
        raise NotImplementedError()

    async def get_geometries(
        self, *, extra: Optional[Dict[str, Any]] = None, timeout: Optional[float] = None
    ) -> Sequence[Geometry]:
        self.logger.error("`get_geometries` is not implemented")
        raise NotImplementedError()

