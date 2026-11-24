import importlib.util
import json
import unittest
from unittest.mock import patch

from test_worker import task_for

from atg_generation_worker.harnesses import PydanticAIHarness


HAS_PINNED_SDK = (
    importlib.util.find_spec("pydantic_ai") is not None
    and importlib.util.find_spec("openai") is not None
    and importlib.util.find_spec("httpx2") is not None
)


@unittest.skipUnless(HAS_PINNED_SDK, "Pydantic AI optional dependencies are not installed")
class PydanticSDKTests(unittest.TestCase):
    def test_pinned_sdk_completes_analyze_and_plan_through_mock_transport(self):
        import httpx2
        import openai

        real_client = openai.AsyncOpenAI
        outputs = {
            "analyze": {
                "summary": "The supplied balance method returns zero.",
                "limitations": [],
                "facts": [{"claim": "balance returns zero", "citations": [{
                    "path": "src/main/java/example/Account.java", "start_line": 2, "end_line": 2
                }]}],
                "gaps": [],
            },
            "plan": {
                "summary": "Test the zero-balance behavior.",
                "limitations": [],
                "scenarios": [{
                    "name": "zero balance",
                    "source_paths": ["src/main/java/example/Account.java"],
                    "test_path": "src/test/java/example/AccountTest.java",
                    "behavior": "Initial account balance is zero.",
                    "oracle": "Assert the returned balance is zero.",
                }],
            },
        }

        for phase, output in outputs.items():
            with self.subTest(phase=phase):
                requests = []

                def respond(request):
                    payload = json.loads(request.content)
                    requests.append((request.url.path, payload))
                    tool_name = payload["tools"][0]["function"]["name"]
                    response_output = {"status": "completed", "output": output, "diagnostic": ""}
                    return httpx2.Response(200, json={
                        "id": "chatcmpl-offline", "object": "chat.completion", "created": 1,
                        "model": payload["model"], "choices": [{
                            "index": 0,
                            "message": {"role": "assistant", "content": None, "tool_calls": [{
                                "id": "call-offline", "type": "function", "function": {
                                    "name": tool_name, "arguments": json.dumps(response_output)
                                }
                            }]},
                            "finish_reason": "tool_calls",
                        }],
                        "usage": {"prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140},
                    })

                class OfflineOpenAI(real_client):
                    def __init__(self, *args, **kwargs):
                        kwargs["http_client"] = httpx2.AsyncClient(transport=httpx2.MockTransport(respond))
                        super().__init__(*args, **kwargs)

                with patch("openai.AsyncOpenAI", OfflineOpenAI):
                    result = PydanticAIHarness().run(
                        task_for(phase), "synthetic-only", allow_model_calls=True
                    )

                self.assertEqual(result.status, "completed")
                self.assertEqual(result.usage, {"input_tokens": 100, "output_tokens": 40, "requests": 1})
                self.assertEqual(len(requests), 1)
                path, payload = requests[0]
                self.assertEqual(path, "/v1/chat/completions")
                self.assertEqual(payload["model"], "fixture")
                self.assertEqual(payload["max_completion_tokens"], 4096)
