"""Offline screening audit or explicitly invoked, bounded real-model evaluation."""
import argparse
import hashlib
import json
import os
import sys
import tempfile
from datetime import datetime,timezone
from pathlib import Path
sys.path.insert(0,str(Path(__file__).resolve().parents[1]))
from lumen.agent import Model
from lumen.capture import PROMPT,parse_items
from lumen.capture_gate import screen
from lumen.core import Store
from lumen.privacy import memory_reasons,reasons


def score(expected,actual,expected_key=None):
    used=set();matched=0
    for want in expected:
        for index,item in enumerate(actual):
            if index in used or item['catalog']!=want['catalog']:continue
            if expected_key and item['key']!=expected_key:continue
            if not all(term.casefold() in item['content'].casefold() for term in want.get('contains',[])):continue
            used.add(index);matched+=1;break
    return {'tp':matched,'fp':len(actual)-matched,'fn':len(expected)-matched}


def main(argv=None):
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--samples',type=Path,default=Path(__file__).resolve().parents[1]/'evaluations/capture_samples.json')
    parser.add_argument('--live',action='store_true',help='明确调用配置的真实模型，产生 API 费用')
    parser.add_argument('--max-calls',type=int,default=60,help='本次评测最多真实调用数量')
    parser.add_argument('--min-precision',type=float,default=0.9,help='真实评测 precision 的最低门槛')
    parser.add_argument('--min-recall',type=float,default=0.9,help='真实评测 recall 的最低门槛')
    parser.add_argument('--output',type=Path,help='报告路径；报告不保存原始输出或密钥')
    args=parser.parse_args(argv)
    if not 1<=args.max_calls<=1000:parser.error('--max-calls 必须为 1–1000')
    if not 0<=args.min_precision<=1 or not 0<=args.min_recall<=1:parser.error('质量门槛必须为 0–1')
    raw=args.samples.read_bytes();dataset=json.loads(raw)
    samples=dataset['samples'];model=Model()
    if args.live and not model.key:
        parser.error('真实评测需要 LUMEN_MODEL_API_KEY 或 LUMEN_DEEPSEEK_API_KEY；当前未配置，未发送请求')
    counts={'tp':0,'fp':0,'fn':0};calls=0;outcomes=[];eligible=0;missed=[]
    with tempfile.TemporaryDirectory(prefix='lumen-evaluation-') as tmp:
        store=Store(tmp+'/evaluation.db');model.store=store
        try:
            for item in samples:
                decision=screen(item['text']);expected=item.get('expected',[])
                should_capture=bool(expected) and item.get('pipeline_capture',True)
                eligible+=int(decision=='candidate')
                if should_capture and decision!='candidate':missed.append(item['id'])
                result={'id':item['id'],'screen':decision}
                if not args.live:result['result']='not_run'
                elif reasons(item['text']):result['result']='sensitive_input_blocked'
                elif calls>=args.max_calls:result['result']='call_limit'
                else:
                    calls+=1
                    try:
                        response=model.classify(item['text'],item.get('memories',[]))
                        start,end=response.find('{'),response.rfind('}')
                        parsed=json.loads(response[start:end+1])
                        if not isinstance(parsed,dict) or not isinstance(parsed.get('items'),list):raise ValueError()
                        actual=parse_items(response)
                        sensitive=any(isinstance(row,dict) and memory_reasons(str(row.get('key','')),str(row.get('content',''))) for row in parsed['items'])
                        invalid=len(actual)!=len(parsed['items'])
                        metrics=score(expected,actual,item.get('expected_key'))
                        for key in counts:counts[key]+=metrics[key]
                        result.update(result='sensitive_output' if sensitive else 'invalid_items' if invalid else 'ok',**metrics)
                    except Exception:
                        # Provider error bodies or malformed outputs may contain private data.
                        result.update(result='request_or_parse_error',fn=len(expected))
                        counts['fn']+=len(expected)
                outcomes.append(result)
        finally:store.db.close()
    report={'mode':'live' if args.live else 'screening_only','model':model.name if args.live else None,
            'created_at':datetime.now(timezone.utc).isoformat(),'dataset_sha256':hashlib.sha256(raw).hexdigest(),
            'prompt_sha256':hashlib.sha256(PROMPT.encode()).hexdigest(),'samples':len(samples),'api_attempts':calls,
            'screen_candidates':eligible,'screen_missed_fact_ids':missed,'classification':None,'outcomes':outcomes}
    if args.live:
        report['classification']={**counts,'precision':counts['tp']/(counts['tp']+counts['fp']) if counts['tp']+counts['fp'] else None,
                                  'recall':counts['tp']/(counts['tp']+counts['fn']) if counts['tp']+counts['fn'] else None}
        complete=all(row['result'] in ('ok','sensitive_input_blocked') for row in outcomes)
        metrics=report['classification']
        report['quality_gate']={'complete':complete,'min_precision':args.min_precision,'min_recall':args.min_recall,
                               'passed':complete and metrics['precision'] is not None and metrics['recall'] is not None
                                        and metrics['precision']>=args.min_precision and metrics['recall']>=args.min_recall}
    if args.output:
        args.output.parent.mkdir(parents=True,exist_ok=True)
        fd=os.open(args.output,os.O_WRONLY|os.O_CREAT|os.O_TRUNC,0o600)
        with os.fdopen(fd,'w') as target:json.dump(report,target,ensure_ascii=False,indent=2)
        os.chmod(args.output,0o600)
    print(json.dumps({key:value for key,value in report.items() if key!='outcomes'},ensure_ascii=False,indent=2))
    return 1 if args.live and not report['quality_gate']['passed'] else 0

if __name__=='__main__':raise SystemExit(main())
