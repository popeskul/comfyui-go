import importlib.util
import unittest
from pathlib import Path

import torch

path = Path(__file__).with_name("__init__.py")
spec = importlib.util.spec_from_file_location("adjust", path)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class AdjustTests(unittest.TestCase):
    def setUp(self):
        self.node = module.LabImageAdjust()
        self.image = torch.tensor([[[[0.2, 0.4, 0.6], [0.8, 0.1, 0.3]]]])

    def test_identity_and_no_input_mutation(self):
        original = self.image.clone()
        (result,) = self.node.adjust(self.image, 0, 1)
        torch.testing.assert_close(result, original)
        torch.testing.assert_close(self.image, original)

    def test_exposure_and_clipping(self):
        (result,) = self.node.adjust(self.image, 1, 1)
        torch.testing.assert_close(result, (self.image * 2).clamp(0, 1))

    def test_zero_saturation_is_grayscale(self):
        (result,) = self.node.adjust(self.image, 0, 0)
        torch.testing.assert_close(result[..., 0], result[..., 1])
        torch.testing.assert_close(result[..., 1], result[..., 2])
        self.assertEqual(result.shape, self.image.shape)
        self.assertEqual(result.dtype, self.image.dtype)

    def test_rejects_invalid_parameters(self):
        for exposure, saturation in [(3, 1), (0, -1), (float("nan"), 1), (0, float("inf"))]:
            with self.subTest(exposure=exposure, saturation=saturation):
                with self.assertRaises(ValueError):
                    self.node.adjust(self.image, exposure, saturation)

    def test_rejects_wrong_tensor_shape(self):
        with self.assertRaises(ValueError):
            self.node.adjust(torch.zeros(3, 512, 512), 0, 1)


class CatalogTests(unittest.TestCase):
    def setUp(self):
        self.node = module.LabCatalogFrame()

    def test_portrait_preserves_content_and_adds_white_padding(self):
        image = torch.zeros(2, 2, 4, 3)
        image[0, :, :, 0] = 1
        image[1, :, :, 2] = 1
        before = image.clone()
        (result,) = self.node.frame(image, "portrait")
        self.assertEqual(result.shape, (2, 960, 768, 3))
        self.assertEqual(result.dtype, image.dtype)
        self.assertEqual(result.device, image.device)
        torch.testing.assert_close(result[:, :288], torch.ones_like(result[:, :288]))
        torch.testing.assert_close(result[:, 672:], torch.ones_like(result[:, 672:]))
        torch.testing.assert_close(result[0, 288:672, :, 0], torch.ones(384, 768))
        torch.testing.assert_close(result[0, 288:672, :, 1:], torch.zeros(384, 768, 2))
        torch.testing.assert_close(result[1, 288:672, :, 2], torch.ones(384, 768))
        torch.testing.assert_close(image, before)

    def test_square_downscales_tall_image_without_cropping(self):
        image = torch.zeros(1, 1024, 512, 3)
        image[:, :512, :, 0] = 1
        image[:, 512:, :, 2] = 1
        (result,) = self.node.frame(image, "square")
        self.assertEqual(result.shape, (1, 768, 768, 3))
        torch.testing.assert_close(result[:, :, :192], torch.ones(1, 768, 192, 3))
        torch.testing.assert_close(result[:, :, 576:], torch.ones(1, 768, 192, 3))
        self.assertGreater(result[0, 0, 384, 0].item(), 0.99)
        self.assertGreater(result[0, -1, 384, 2].item(), 0.99)

    def test_original_preserves_shape_and_values(self):
        image = torch.rand(1, 5, 7, 3)
        (result,) = self.node.frame(image, "original")
        torch.testing.assert_close(result, image)

    def test_rejects_invalid_format_and_tensor(self):
        with self.assertRaises(ValueError):
            self.node.frame(torch.zeros(1, 4, 4, 3), "unknown")
        for image in [
            torch.zeros(3, 4, 4),
            torch.zeros(1, 0, 4, 3),
            torch.zeros(1, 4, 4, 4),
            torch.zeros(1, 4, 4, 3, dtype=torch.uint8),
        ]:
            with self.subTest(shape=image.shape, dtype=image.dtype):
                with self.assertRaises(ValueError):
                    self.node.frame(image, "square")


if __name__ == "__main__":
    unittest.main()
