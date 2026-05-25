import argparse
import signal
import sys
from concurrent import futures

import grpc

from pb import runtime_pb2_grpc
from server import PythonRuntime


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=9092)
    parser.add_argument("--work-dir", default="/workspace")
    args = parser.parse_args()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
    runtime_pb2_grpc.add_RuntimeServicer_to_server(
        PythonRuntime(work_dir=args.work_dir), server
    )
    server.add_insecure_port(f"[::]:{args.port}")
    server.start()
    print(f"Python runtime listening on port {args.port}", flush=True)

    def shutdown(sig, frame):
        server.stop(0)
        sys.exit(0)

    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    signal.pause()


if __name__ == "__main__":
    main()
