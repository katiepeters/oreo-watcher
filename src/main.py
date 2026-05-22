import asyncio
from viam.module.module import Module
try:
    # from models.motion_cam import MotionCam
    from models.filtered_mic import FilteredMic
except ModuleNotFoundError:
    # when running as local module with run.sh
    # from .models.motion_cam import MotionCam
    from .models.filtered_mic import FilteredMic


if __name__ == '__main__':
    asyncio.run(Module.run_from_registry())
