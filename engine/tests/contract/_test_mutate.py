import sys, os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), 'libs', 'proto-gen', 'python'))
import codeaudit_common_pb2 as pb2

req = pb2.BatchCreateFindingsRequest()
req.metadata.request_id = 'test'
req.metadata.caller_service = 'test'
print('Before:', len(req.SerializeToString()), 'bytes')

try:
    elem = req.findings.add()
    elem.finding_id = 'marker'
    print('After add:', len(req.SerializeToString()), 'bytes')
    print('Success!')
except Exception as e:
    print('Add failed:', type(e).__name__, e)
