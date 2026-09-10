"""Run actual CPA v7.2.145 scheduling contracts in a disposable source tree.

Requires Go >=1.26, CGO for the plugin tests, Python >=3.10 and network for
the pinned CPA source/dependencies. No service or real credential is used.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parents[1]
TAG = "v7.2.145"


def run():
    with tempfile.TemporaryDirectory(prefix="token-usage-cpa-") as temporary:
        target = Path(temporary)
        archive = target / "cpa.zip"
        urllib.request.urlretrieve(
            f"https://codeload.github.com/router-for-me/CLIProxyAPI/zip/refs/tags/{TAG}", archive
        )
        with zipfile.ZipFile(archive) as source:
            for entry in source.infolist():
                if not (target / entry.filename).resolve().is_relative_to(target.resolve()):
                    raise RuntimeError("invalid archive path")
            source.extractall(target)
        cpa = target / "CLIProxyAPI-7.2.145"
        fixture = target / "native-response.json"
        env = dict(os.environ, CPA_NATIVE_RESPONSE_FIXTURE=str(fixture))
        subprocess.run(["go", "test", "-count=1", "-run", "^TestNativeProtocolFixture$", "."], cwd=ROOT, env=env, check=True)
        shutil.copyfile(ROOT / "integration/cpa_native_test.go.fixture", cpa / "sdk/cliproxy/auth/token_usage_native_test.go")
        pattern = "TokenUsageNative|Weighted|Priority|SessionAffinity|Excluded|PluginSchedulerFallback"
        subprocess.run(["go", "test", "-count=1", "-run", pattern, "./sdk/cliproxy/auth"], cwd=cpa, env=env, check=True)
        subprocess.run(["go", "test", "-count=1", "-run", "ExcludedModels", "./internal/watcher/synthesizer"], cwd=cpa, env=env, check=True)
        # Exercise the real status handler, including name+index protection.
        subprocess.run(["go", "test", "-count=1", "-run", "PatchAuthFileStatus", "./internal/api/handlers/management"], cwd=cpa, env=env, check=True)


if __name__ == "__main__":
    run()
