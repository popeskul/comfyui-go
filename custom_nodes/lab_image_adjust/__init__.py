import math

import torch
import torch.nn.functional as F


class LabImageAdjust:
    """Apply exposure and saturation to ComfyUI's BHWC float image tensor."""

    @classmethod
    def INPUT_TYPES(cls):
        return {
            "required": {
                "image": ("IMAGE",),
                "exposure_stops": ("FLOAT", {"default": 0.0, "min": -2.0, "max": 2.0, "step": 0.1}),
                "saturation": ("FLOAT", {"default": 1.0, "min": 0.0, "max": 2.0, "step": 0.1}),
            }
        }

    RETURN_TYPES = ("IMAGE",)
    FUNCTION = "adjust"
    CATEGORY = "local_lab"

    def adjust(self, image, exposure_stops, saturation):
        if image.ndim != 4 or image.shape[-1] != 3:
            raise ValueError("Expected a BHWC RGB image tensor")
        if not math.isfinite(exposure_stops) or not -2 <= exposure_stops <= 2:
            raise ValueError("exposure_stops must be finite and between -2 and 2")
        if not math.isfinite(saturation) or not 0 <= saturation <= 2:
            raise ValueError("saturation must be finite and between 0 and 2")
        # Simple display-space adjustment, not a color-managed HDR operation.
        exposed = image * (2.0**exposure_stops)
        weights = image.new_tensor([0.2126, 0.7152, 0.0722])
        gray = (exposed * weights).sum(dim=-1, keepdim=True)
        return ((gray + (exposed - gray) * saturation).clamp(0.0, 1.0),)


class LabCatalogFrame:
    """Fit the entire image inside a white catalog canvas without cropping."""

    @classmethod
    def INPUT_TYPES(cls):
        return {
            "required": {
                "image": ("IMAGE",),
                "format": (["original", "square", "portrait"],),
            }
        }

    RETURN_TYPES = ("IMAGE",)
    FUNCTION = "frame"
    CATEGORY = "local_lab"

    def frame(self, image, format):
        if image.ndim != 4 or image.shape[-1] != 3 or min(image.shape) < 1:
            raise ValueError("Expected a non-empty BHWC RGB image tensor")
        if not torch.is_floating_point(image):
            raise ValueError("Expected a floating-point image tensor")
        if format == "original":
            return (image,)
        if format not in ("square", "portrait"):
            raise ValueError("Unknown catalog format")

        width, height = (768, 768) if format == "square" else (768, 960)
        batch, source_height, source_width, _ = image.shape
        scale = min(width / source_width, height / source_height)
        resized_width = max(1, min(width, round(source_width * scale)))
        resized_height = max(1, min(height, round(source_height * scale)))
        resized = (
            F.interpolate(
                image.movedim(-1, 1).float(),
                size=(resized_height, resized_width),
                mode="bilinear",
                align_corners=False,
                antialias=True,
            )
            .movedim(1, -1)
            .to(image.dtype)
        )

        canvas = image.new_ones((batch, height, width, 3))
        top = (height - resized_height) // 2
        left = (width - resized_width) // 2
        canvas[:, top : top + resized_height, left : left + resized_width] = resized
        return (canvas,)


NODE_CLASS_MAPPINGS = {
    "LabImageAdjust": LabImageAdjust,
    "LabCatalogFrame": LabCatalogFrame,
}
NODE_DISPLAY_NAME_MAPPINGS = {
    "LabImageAdjust": "Lab: Exposure and Saturation",
    "LabCatalogFrame": "Lab: Catalog Frame",
}
