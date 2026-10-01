from datetime import datetime, timezone
import uuid

S = {'type': 'string'}


def now():
    return datetime.now(timezone.utc)


def stamp(value=None):
    return (value or now()).isoformat()


def identifier():
    return uuid.uuid4().hex


def tool(name, description, properties, required=()):
    return {'type':'function','function':{'name':name,'description':description,
        'parameters':{'type':'object','properties':properties,'required':list(required),'additionalProperties':False}}}
