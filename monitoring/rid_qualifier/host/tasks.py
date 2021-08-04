import json
import time
import redis
import rq
from . import config
from monitoring.monitorlib.typing import ImplicitDict
from monitoring.rid_qualifier import test_executor
from monitoring.rid_qualifier.utils import RIDQualifierTestConfiguration
from monitoring.rid_qualifier.test_data import test_report
from rq import get_current_job


def get_rq_job(job_id):
  try:
      rq_job = config.Config.qualifier_queue.fetch_job(job_id)
  except (redis.exceptions.RedisError, rq.exceptions.NoSuchJobError):
      return None
  return rq_job


def call_test_executor(user_config_json, auth_spec, input_files, debug=False):
    user_config: RIDQualifierTestConfiguration = ImplicitDict.parse(
        json.loads(user_config_json), RIDQualifierTestConfiguration)
    if debug:
        report = test_report.test_data
    else:
        report = test_executor.main(user_config, auth_spec, input_files)
    if report:
        job = get_current_job()
        job_id = job.get_id()
        if job_id:
            config.Config.redis_client.set(job_id, json.dumps(report))