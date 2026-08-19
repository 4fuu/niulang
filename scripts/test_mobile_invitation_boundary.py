import pathlib
import plistlib
import unittest
import xml.etree.ElementTree as element_tree


ROOT = pathlib.Path(__file__).resolve().parents[1]
ANDROID_ATTRIBUTE = "{http://schemas.android.com/apk/res/android}"


class MobileInvitationBoundaryTests(unittest.TestCase):
    def test_ios_does_not_claim_the_bearer_invitation_scheme(self):
        info_path = ROOT / "mobile/ios/Queqiao/Info.plist"
        with info_path.open("rb") as source:
            info = plistlib.load(source)
        self.assertNotIn("CFBundleURLTypes", info)

        project = (ROOT / "mobile/ios/project.yml").read_text(encoding="utf-8")
        self.assertNotIn("CFBundleURLTypes", project)
        self.assertNotIn("CFBundleURLSchemes", project)

    def test_android_requires_explicit_paste_or_share_selection(self):
        manifest = element_tree.parse(
            ROOT / "mobile/android/app/src/main/AndroidManifest.xml"
        )
        actions = {
            action.get(ANDROID_ATTRIBUTE + "name")
            for action in manifest.findall(".//intent-filter/action")
        }
        categories = {
            category.get(ANDROID_ATTRIBUTE + "name")
            for category in manifest.findall(".//intent-filter/category")
        }
        schemes = {
            data.get(ANDROID_ATTRIBUTE + "scheme")
            for data in manifest.findall(".//intent-filter/data")
        }

        self.assertIn("android.intent.action.SEND", actions)
        self.assertNotIn("android.intent.action.VIEW", actions)
        self.assertNotIn("android.intent.category.BROWSABLE", categories)
        self.assertNotIn("queqiao", schemes)


if __name__ == "__main__":
    unittest.main()
