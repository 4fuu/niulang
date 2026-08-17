import unittest

from scripts import field_soak


class FieldSoakTests(unittest.TestCase):
    def test_https_response_requires_a_success_status(self):
        status, body = field_soak.parse_https_response(
            b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"
        )
        self.assertEqual((status, body), (200, b"ok"))
        for response in (
            b"incomplete",
            b"HTTP/1.1 nope Bad\r\n\r\n",
            b"HTTP/1.1 503 Bad\r\n\r\n",
        ):
            with self.subTest(response=response):
                with self.assertRaises(RuntimeError):
                    field_soak.parse_https_response(response)

    def test_socks_reply_rejects_failure(self):
        class Connection:
            def __init__(self):
                self.data = bytearray(b"\x05\x05\x00\x01")

            def recv(self, size):
                result = self.data[:size]
                del self.data[:size]
                return bytes(result)

        with self.assertRaises(RuntimeError):
            field_soak.read_socks_reply(Connection())

    def test_resource_settlement_requires_flow_replay_and_descriptors_to_return(self):
        metrics = {"queqiao_active_flows": 0, "queqiao_replay_bytes_in_use": 0}
        process = {"file_descriptors": 10}
        self.assertTrue(field_soak.resources_settled(metrics, metrics, process, process))
        self.assertFalse(
            field_soak.resources_settled(
                metrics, {**metrics, "queqiao_active_flows": 1}, process, process
            )
        )


if __name__ == "__main__":
    unittest.main()
