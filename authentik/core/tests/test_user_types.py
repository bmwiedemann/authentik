"""Account restrictions shared by features and queryset filters."""

from itertools import product

from django.test import TestCase

from authentik.core.models import Actor, ActorPolicyInheritance, User, UserTypes
from authentik.core.user_types import matches_user_type
from authentik.enterprise.agents.models import Agent


class UserRestrictionTests(TestCase):
    """Exercise each category independently and in combination."""

    @classmethod
    def setUpTestData(cls):
        cls.internal = User.objects.create(username="internal")
        cls.external = User.objects.create(username="external", type=UserTypes.EXTERNAL)
        cls.service = User.objects.create(username="service", type=UserTypes.SERVICE_ACCOUNT)
        cls.internal_service = User.objects.create(
            username="internal-service", type=UserTypes.INTERNAL_SERVICE_ACCOUNT
        )
        cls.actor = Actor.for_user(cls.internal, ActorPolicyInheritance.NONE)
        cls.agent = Agent.create_for_user(cls.internal)
        cls.categories = {
            cls.internal.pk: None,
            cls.external.pk: None,
            cls.service.pk: "service_accounts",
            cls.internal_service.pk: "internal_service_accounts",
            cls.actor.pk: "service_accounts",
            cls.agent.pk: "agents",
        }

    def test_matches_user_type(self):
        """Instance checks distinguish agents from plain actors and service accounts."""
        users = list(User.objects.filter(pk__in=self.categories).select_related("actor__agent"))
        for values in product((False, True), repeat=3):
            flags = dict(
                zip(
                    ("service_accounts", "internal_service_accounts", "agents"), values, strict=True
                )
            )
            for user in [*users, self.agent, self.actor]:
                with self.subTest(flags=flags, user=user.username):
                    expected = flags.get(self.categories[user.pk], False)
                    self.assertEqual(matches_user_type(user, **flags), expected)

    def test_exclude_user_types(self):
        """Manager and chained queryset calls retain precisely the allowed categories."""
        for values in product((False, True), repeat=3):
            flags = dict(
                zip(
                    ("service_accounts", "internal_service_accounts", "agents"), values, strict=True
                )
            )
            expected = {
                pk for pk, category in self.categories.items() if not flags.get(category, False)
            }
            for users in (User.objects, User.objects.filter(pk__in=self.categories)):
                with self.subTest(flags=flags, entrypoint=type(users)), self.assertNumQueries(1):
                    self.assertSetEqual(
                        set(
                            users.exclude_user_types(**flags)
                            .filter(pk__in=self.categories)
                            .values_list("pk", flat=True)
                        ),
                        expected,
                    )
