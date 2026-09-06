"""
SMK-6 冒烟测试配置
依据: test-gates.md §5 SMK-6
"""

import os
import pytest


def pytest_addoption(parser):
    """添加命令行选项"""
    parser.addoption(
        "--gateway-url",
        default=os.getenv("GATEWAY_URL", "http://localhost:18080"),
        help="Gateway URL"
    )
    parser.addoption(
        "--project-service-url",
        default=os.getenv("PROJECT_SERVICE_URL", "http://localhost:50052"),
        help="Project Service URL"
    )
    parser.addoption(
        "--task-service-url",
        default=os.getenv("TASK_SERVICE_URL", "http://localhost:50054"),
        help="Task Service URL"
    )
    parser.addoption(
        "--result-service-url",
        default=os.getenv("RESULT_SERVICE_URL", "http://localhost:50058"),  # ADR-117
        help="Result Service URL"
    )


@pytest.fixture(scope="session")
def gateway_url(request):
    """Gateway URL fixture"""
    return request.config.getoption("--gateway-url")


@pytest.fixture(scope="session")
def project_service_url(request):
    """Project Service URL fixture"""
    return request.config.getoption("--project-service-url")


@pytest.fixture(scope="session")
def task_service_url(request):
    """Task Service URL fixture"""
    return request.config.getoption("--task-service-url")


@pytest.fixture(scope="session")
def result_service_url(request):
    """Result Service URL fixture"""
    return request.config.getoption("--result-service-url")
